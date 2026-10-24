package service

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"

// Flag keys are owned by identity and files, which this module cannot
// import. The test binary registers identical stand-ins under the same owners
// so the admin service sees a realistic catalog.
const (
	testKeyUserRegistration features.Key = "user_registration"
	testKeyGoogleOAuth      features.Key = "google_oauth"
	testKeyFileUploads      features.Key = "file_uploads"
)

func init() {
	if err := features.Register("identity",
		features.Definition{Key: testKeyUserRegistration, Description: "Allow new users to register through the public API.", Default: true},
		features.Definition{Key: testKeyGoogleOAuth, Description: "Expose Google OAuth login and callback routes (still requires Google credentials).", Default: true},
	); err != nil {
		panic(err)
	}
	if err := features.Register("files",
		features.Definition{Key: testKeyFileUploads, Description: "Allow authenticated file uploads.", Default: true},
	); err != nil {
		panic(err)
	}
}
