package cloudprovider

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	oadpv1alpha1 "github.com/openshift/oadp-operator/api/v1alpha1"
)

// CloudProvider defines operations supported by each cloud.
type CloudProvider interface {
	// UploadTest performs a test upload and returns calculated speed and test duration
	UploadTest(ctx context.Context, config oadpv1alpha1.UploadSpeedTestConfig, bucket string, log logr.Logger) (int64, time.Duration, error)

	// GetBucketMetadata retrieves the encryption and versioning config for a bucket
	GetBucketMetadata(ctx context.Context, bucket string, log logr.Logger) (*oadpv1alpha1.BucketMetadata, error)
}
