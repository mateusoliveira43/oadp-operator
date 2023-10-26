package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/openshift/oadp-operator/tests/e2e/lib"
	corev1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type VerificationFunction func(client.Client, string) error

type appVerificationFunction func(bool, bool, BackupRestoreType) VerificationFunction

func mongoready(preBackupState bool, twoVol bool, backupRestoreType BackupRestoreType) VerificationFunction {
	return VerificationFunction(func(ocClient client.Client, namespace string) error {
		Eventually(IsDCReady(ocClient, namespace, "todolist"), timeoutMultiplier*time.Minute*10, time.Second*10).Should(BeTrue())
		exists, err := DoesSCCExist(ocClient, "mongo-persistent-scc")
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("did not find Mongo scc")
		}
		err = VerifyBackupRestoreData(artifact_dir, namespace, "todolist-route", "todolist", preBackupState, false, backupRestoreType)
		return err
	})
}

// TODO duplications with mongoready
func mysqlReady(preBackupState bool, twoVol bool, backupRestoreType BackupRestoreType) VerificationFunction {
	return VerificationFunction(func(ocClient client.Client, namespace string) error {
		GinkgoWriter.Printf("checking for the NAMESPACE: %s\n", namespace)
		// This test confirms that SCC restore logic in our plugin is working
		Eventually(IsDeploymentReady(ocClient, namespace, "mysql"), timeoutMultiplier*time.Minute*10, time.Second*10).Should(BeTrue())
		exists, err := DoesSCCExist(ocClient, "mysql-persistent-scc")
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("did not find MYSQL scc")
		}
		err = VerifyBackupRestoreData(artifact_dir, namespace, "todolist-route", "todolist", preBackupState, twoVol, backupRestoreType)
		return err
	})
}

type BackupRestoreCase struct {
	ApplicationTemplate          string
	ApplicationNamespace         string
	Name                         string
	BackupRestoreType            BackupRestoreType
	PreBackupVerify              VerificationFunction
	PostRestoreVerify            VerificationFunction
	AppReadyDelay                time.Duration
	MaxK8SVersion                *K8sVersion
	MinK8SVersion                *K8sVersion
	MustGatherFiles              []string            // list of files expected in must-gather under quay.io.../clusters/clustername/... ie. "namespaces/openshift-adp/oadp.openshift.io/dpa-ts-example-velero/ts-example-velero.yml"
	MustGatherValidationFunction *func(string) error // validation function for must-gather where string parameter is the path to "quay.io.../clusters/clustername/"
}

func runBackupAndRestore(brCase BackupRestoreCase, expectedErr error, updateLastBRcase func(brCase BackupRestoreCase), updateLastInstallTime func()) {
	if provider == "azure" && (brCase.BackupRestoreType == CSI || brCase.BackupRestoreType == CSIDataMover) {
		if brCase.MinK8SVersion == nil {
			brCase.MinK8SVersion = &K8sVersion{Major: "1", Minor: "23"}
		}
	}
	if notVersionTarget, reason := NotServerVersionTarget(brCase.MinK8SVersion, brCase.MaxK8SVersion); notVersionTarget {
		Skip(reason)
	}

	updateLastBRcase(brCase)

	err := dpaCR.Build(brCase.BackupRestoreType)
	Expect(err).NotTo(HaveOccurred())

	updateLastInstallTime()

	err = dpaCR.CreateOrUpdate(&dpaCR.CustomResource.Spec)
	Expect(err).NotTo(HaveOccurred())

	GinkgoWriter.Println("Waiting for velero pod to be running")
	Eventually(AreVeleroPodsRunning(namespace), timeoutMultiplier*time.Minute*3, time.Second*5).Should(BeTrue())

	if brCase.BackupRestoreType == RESTIC || brCase.BackupRestoreType == CSIDataMover {
		GinkgoWriter.Println("Waiting for Node Agent pods to be running")
		Eventually(AreNodeAgentPodsRunning(namespace), timeoutMultiplier*time.Minute*3, time.Second*5).Should(BeTrue())
	}
	if brCase.BackupRestoreType == CSI || brCase.BackupRestoreType == CSIDataMover {
		GinkgoWriter.Printf("Creating VolumeSnapshotClass for CSI backuprestore of %s\n", brCase.Name)
		snapshotClassPath := fmt.Sprintf("./sample-applications/snapclass-csi/%s.yaml", provider)
		err = InstallApplication(dpaCR.Client, snapshotClassPath)
		Expect(err).ToNot(HaveOccurred())
	}

	// TODO: check registry deployments are deleted
	// TODO: check S3 for images

	backupUid, _ := uuid.NewUUID()
	restoreUid, _ := uuid.NewUUID()
	backupName := fmt.Sprintf("%s-%s", brCase.Name, backupUid.String())
	restoreName := fmt.Sprintf("%s-%s", brCase.Name, restoreUid.String())

	// install app
	updateLastInstallTime()
	GinkgoWriter.Printf("Installing application for case %s\n", brCase.Name)
	err = InstallApplication(dpaCR.Client, brCase.ApplicationTemplate)
	Expect(err).ToNot(HaveOccurred())
	if brCase.BackupRestoreType == CSI || brCase.BackupRestoreType == CSIDataMover {
		GinkgoWriter.Printf("Creating pvc for case %s\n", brCase.Name)
		var pvcPath string
		if strings.Contains(brCase.Name, "twovol") {
			pvcPath = fmt.Sprintf("./sample-applications/%s/pvc-twoVol/%s.yaml", brCase.ApplicationNamespace, provider)
		} else {
			pvcPath = fmt.Sprintf("./sample-applications/%s/pvc/%s.yaml", brCase.ApplicationNamespace, provider)
		}
		err = InstallApplication(dpaCR.Client, pvcPath)
		Expect(err).ToNot(HaveOccurred())
	}

	// wait for pods to be running
	Eventually(AreAppBuildsReady(dpaCR.Client, brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*5, time.Second*5).Should(BeTrue())
	Eventually(AreApplicationPodsRunning(brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*9, time.Second*5).Should(BeTrue())

	// Run optional custom verification
	GinkgoWriter.Printf("Running pre-backup function for case %s\n", brCase.Name)
	err = brCase.PreBackupVerify(dpaCR.Client, brCase.ApplicationNamespace)
	Expect(err).ToNot(HaveOccurred())

	nsRequiresResticDCWorkaround, err := NamespaceRequiresResticDCWorkaround(dpaCR.Client, brCase.ApplicationNamespace)
	Expect(err).ToNot(HaveOccurred())

	GinkgoWriter.Printf("Sleeping for %v to allow application to be ready for case %s\n", brCase.AppReadyDelay, brCase.Name)
	time.Sleep(brCase.AppReadyDelay)
	// create backup
	GinkgoWriter.Printf("Creating backup %s for case %s\n", backupName, brCase.Name)
	backup, err := CreateBackupForNamespaces(dpaCR.Client, namespace, backupName, []string{brCase.ApplicationNamespace}, brCase.BackupRestoreType == RESTIC, brCase.BackupRestoreType == CSIDataMover)
	Expect(err).ToNot(HaveOccurred())

	// wait for backup to not be running
	Eventually(IsBackupDone(dpaCR.Client, namespace, backupName), timeoutMultiplier*time.Minute*20, time.Second*10).Should(BeTrue())
	// TODO only when failed?
	// GinkgoWriter.Println(DescribeBackup(dpaCR.Client, backup))
	Expect(BackupErrorLogs(dpaCR.Client, backup)).To(Equal([]string{}))

	// check if backup succeeded
	succeeded, err := IsBackupCompletedSuccessfully(dpaCR.Client, backup)
	Expect(err).ToNot(HaveOccurred())
	Expect(succeeded).To(Equal(true))
	GinkgoWriter.Printf("Backup for case %s succeeded\n", brCase.Name)

	if brCase.BackupRestoreType == CSI {
		// wait for volume snapshot to be Ready
		Eventually(AreVolumeSnapshotsReady(dpaCR.Client, backupName), timeoutMultiplier*time.Minute*4, time.Second*10).Should(BeTrue())
	}

	// uninstall app
	GinkgoWriter.Printf("Uninstalling app for case %s\n", brCase.Name)
	err = UninstallApplication(dpaCR.Client, brCase.ApplicationTemplate)
	Expect(err).ToNot(HaveOccurred())

	// Wait for namespace to be deleted
	Eventually(IsNamespaceDeleted(brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*4, time.Second*5).Should(BeTrue())

	updateLastInstallTime()
	// run restore
	GinkgoWriter.Printf("Creating restore %s for case %s\n", restoreName, brCase.Name)
	restore, err := CreateRestoreFromBackup(dpaCR.Client, namespace, backupName, restoreName)
	Expect(err).ToNot(HaveOccurred())
	Eventually(IsRestoreDone(dpaCR.Client, namespace, restoreName), timeoutMultiplier*time.Minute*60, time.Second*10).Should(BeTrue())
	// TODO only when failed?
	// GinkgoWriter.Println(DescribeRestore(dpaCR.Client, restore))
	Expect(RestoreErrorLogs(dpaCR.Client, restore)).To(Equal([]string{}))

	// Check if restore succeeded
	succeeded, err = IsRestoreCompletedSuccessfully(dpaCR.Client, namespace, restoreName)
	Expect(err).ToNot(HaveOccurred())
	Expect(succeeded).To(Equal(true))

	if brCase.BackupRestoreType == RESTIC && nsRequiresResticDCWorkaround {
		// run the restic post restore script if restore type is RESTIC
		GinkgoWriter.Printf("Running restic post restore script for case %s\n", brCase.Name)
		err = RunResticPostRestoreScript(restoreName)
		Expect(err).ToNot(HaveOccurred())
	}

	// verify app is running
	Eventually(AreAppBuildsReady(dpaCR.Client, brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*3, time.Second*5).Should(BeTrue())
	Eventually(AreApplicationPodsRunning(brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*9, time.Second*5).Should(BeTrue())

	// Run optional custom verification
	GinkgoWriter.Printf("Running post-restore function for case %s\n", brCase.Name)
	err = brCase.PostRestoreVerify(dpaCR.Client, brCase.ApplicationNamespace)
	Expect(err).ToNot(HaveOccurred())
}

func tearDownBackupAndRestore(brCase BackupRestoreCase, installTime time.Time, report SpecReport) {
	GinkgoWriter.Println("Post backup and restore state: ", report.State.String())
	if report.Failed() {
		// print namespace error events for app namespace
		if brCase.ApplicationNamespace != "" {
			GinkgoWriter.Printf("Printing application namespace %v events after %v\n", brCase.ApplicationNamespace, installTime.Format("15:04:05"))
			PrintNamespaceEventsAfterTime(brCase.ApplicationNamespace, installTime)
		}
		GinkgoWriter.Printf("Printing OADP operator namespace %v events after %v\n", namespace, installTime.Format("15:04:05"))
		PrintNamespaceEventsAfterTime(namespace, installTime)
		baseReportDir := artifact_dir + "/" + report.LeafNodeText
		err := os.MkdirAll(baseReportDir, 0755)
		Expect(err).NotTo(HaveOccurred())
		err = SavePodLogs(namespace, baseReportDir)
		Expect(err).NotTo(HaveOccurred())
		err = SavePodLogs(brCase.ApplicationNamespace, baseReportDir)
		Expect(err).NotTo(HaveOccurred())
	}

	if brCase.BackupRestoreType == CSI || brCase.BackupRestoreType == CSIDataMover {
		GinkgoWriter.Printf("Deleting VolumeSnapshot for CSI backup and restore of %s\n", brCase.Name)
		snapshotClassPath := fmt.Sprintf("./sample-applications/snapclass-csi/%s.yaml", provider)
		err := UninstallApplication(dpaCR.Client, snapshotClassPath)
		Expect(err).ToNot(HaveOccurred())
	}

	err := dpaCR.Client.Delete(context.Background(), &corev1.Namespace{ObjectMeta: v1.ObjectMeta{
		Name:      brCase.ApplicationNamespace,
		Namespace: brCase.ApplicationNamespace,
	}}, &client.DeleteOptions{})
	if k8serror.IsNotFound(err) {
		err = nil
	}
	Expect(err).ToNot(HaveOccurred())

	err = dpaCR.Delete()
	Expect(err).ToNot(HaveOccurred())
	Eventually(IsNamespaceDeleted(brCase.ApplicationNamespace), timeoutMultiplier*time.Minute*2, time.Second*5).Should(BeTrue())
}

var _ = Describe("Backup and restore tests", func() {
	var lastBRCase BackupRestoreCase
	var lastInstallTime time.Time
	updateLastBRcase := func(brCase BackupRestoreCase) {
		lastBRCase = brCase
	}
	updateLastInstallTime := func() {
		lastInstallTime = time.Now()
	}

	var _ = BeforeEach(func() {
		dpaCR.Name = "ts-" + instanceName
	})

	var _ = AfterEach(func(ctx SpecContext) {
		tearDownBackupAndRestore(lastBRCase, lastInstallTime, ctx.SpecReport())
	})

	DescribeTable("Backup and restore applications",
		func(brCase BackupRestoreCase, expectedErr error) {
			runBackupAndRestore(brCase, expectedErr, updateLastBRcase, updateLastInstallTime)
		},
		Entry("MySQL application CSI", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mysql-persistent/mysql-persistent-csi.yaml",
			ApplicationNamespace: "mysql-persistent",
			Name:                 "mysql-csi-e2e",
			BackupRestoreType:    CSI,
			PreBackupVerify:      mysqlReady(true, false, CSI),
			PostRestoreVerify:    mysqlReady(false, false, CSI),
		}, nil),
		Entry("Mongo application CSI", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mongo-persistent/mongo-persistent-csi.yaml",
			ApplicationNamespace: "mongo-persistent",
			Name:                 "mongo-csi-e2e",
			BackupRestoreType:    CSI,
			PreBackupVerify:      mongoready(true, false, CSI),
			PostRestoreVerify:    mongoready(false, false, CSI),
		}, nil),
		Entry("MySQL application two Vol CSI", BackupRestoreCase{
			ApplicationTemplate:  fmt.Sprintf("./sample-applications/mysql-persistent/mysql-persistent-twovol-csi.yaml"),
			ApplicationNamespace: "mysql-persistent",
			Name:                 "mysql-twovol-csi-e2e",
			BackupRestoreType:    CSI,
			AppReadyDelay:        30 * time.Second,
			PreBackupVerify:      mysqlReady(true, true, CSI),
			PostRestoreVerify:    mysqlReady(false, true, CSI),
		}, nil),
		Entry("Mongo application RESTIC", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mongo-persistent/mongo-persistent.yaml",
			ApplicationNamespace: "mongo-persistent",
			Name:                 "mongo-restic-e2e",
			BackupRestoreType:    RESTIC,
			PreBackupVerify:      mongoready(true, false, RESTIC),
			PostRestoreVerify:    mongoready(false, false, RESTIC),
		}, nil),
		Entry("MySQL application RESTIC", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mysql-persistent/mysql-persistent.yaml",
			ApplicationNamespace: "mysql-persistent",
			Name:                 "mysql-restic-e2e",
			BackupRestoreType:    RESTIC,
			PreBackupVerify:      mysqlReady(true, false, RESTIC),
			PostRestoreVerify:    mysqlReady(false, false, RESTIC),
		}, nil),
		Entry("Mongo application DATAMOVER", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mongo-persistent/mongo-persistent-csi.yaml",
			ApplicationNamespace: "mongo-persistent",
			Name:                 "mongo-datamover-e2e",
			BackupRestoreType:    CSIDataMover,
			PreBackupVerify:      mongoready(true, false, CSIDataMover),
			PostRestoreVerify:    mongoready(false, false, CSIDataMover),
		}, nil),
		Entry("MySQL application DATAMOVER", BackupRestoreCase{
			ApplicationTemplate:  "./sample-applications/mysql-persistent/mysql-persistent-csi.yaml",
			ApplicationNamespace: "mysql-persistent",
			Name:                 "mysql-datamover-e2e",
			BackupRestoreType:    CSIDataMover,
			PreBackupVerify:      mysqlReady(true, false, CSIDataMover),
			PostRestoreVerify:    mysqlReady(false, false, CSIDataMover),
		}, nil),
	)
})
