package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"

// KeyFileUploads gates authenticated file uploads.
const KeyFileUploads features.Key = "file_uploads"

// FeatureFlags returns the files flags for app.FlagProvider.
func FeatureFlags() []features.Definition {
	return []features.Definition{
		{
			Key:         KeyFileUploads,
			Description: "Allow authenticated file uploads.",
			Default:     true,
		},
	}
}
