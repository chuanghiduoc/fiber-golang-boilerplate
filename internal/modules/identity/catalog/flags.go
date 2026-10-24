package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"

// Feature flags owned by identity.
const (
	KeyUserRegistration  features.Key = "user_registration"
	KeyEmailVerification features.Key = "email_verification"
	KeyGoogleOAuth       features.Key = "google_oauth"
)

// FeatureFlags returns identity's flags for app.FlagProvider.
func FeatureFlags() []features.Definition {
	return []features.Definition{
		{
			Key:         KeyUserRegistration,
			Description: "Allow new users to register through the public API.",
			Default:     true,
		},
		{
			Key:         KeyEmailVerification,
			Description: "Require email verification before authenticated actions.",
			Default:     false,
		},
		{
			Key:         KeyGoogleOAuth,
			Description: "Expose Google OAuth login and callback routes (still requires Google credentials).",
			Default:     true,
		},
	}
}
