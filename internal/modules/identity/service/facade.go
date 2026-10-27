package service

import (
	authn "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	membership "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	mfa "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	privacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/privacy"
	tenancy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
)

type AccessClaims = authn.AccessClaims
type AccessTokenSigner = authn.AccessTokenSigner
type Admission = authn.Admission
type ChangePasswordInput = authn.ChangePasswordInput
type EmailVerificationService = authn.EmailVerificationService
type ForgotPasswordInput = authn.ForgotPasswordInput
type GoogleSignIn = authn.GoogleSignIn
type GoogleSignInResult = authn.GoogleSignInResult
type LoginInput = authn.LoginInput
type PasswordResetEmailLookup = authn.PasswordResetEmailLookup
type PasswordResetService = authn.PasswordResetService
type RefreshResult = authn.RefreshResult
type RegisterInput = authn.RegisterInput
type ResetDeps = authn.ResetDeps
type ResetPasswordInput = authn.ResetPasswordInput
type ResetRequestQueue = authn.ResetRequestQueue
type SecurityEvent = authn.SecurityEvent
type SecurityNotice = authn.SecurityNotice
type SecurityNotifier = authn.SecurityNotifier
type SessionService = authn.SessionService
type TokenPurger = authn.TokenPurger
type UpdateProfileInput = authn.UpdateProfileInput
type UserDeps = authn.UserDeps
type UserService = authn.UserService
type VerificationDeps = authn.VerificationDeps

const MaxMFAAttempts = authn.MaxMFAAttempts
const ResetRequestJobKind = authn.ResetRequestJobKind
const SecurityEventEmailChanged = authn.SecurityEventEmailChanged
const SecurityEventMFADisabled = authn.SecurityEventMFADisabled
const SecurityEventMFAEnabled = authn.SecurityEventMFAEnabled
const SecurityEventMFAReset = authn.SecurityEventMFAReset
const SecurityEventPasskeyAdded = authn.SecurityEventPasskeyAdded
const SecurityEventPasskeyRemoved = authn.SecurityEventPasskeyRemoved
const SecurityEventPasswordChanged = authn.SecurityEventPasswordChanged
const SecurityEventPasswordReset = authn.SecurityEventPasswordReset
const SecurityEventRecoveryCodesRegenerated = authn.SecurityEventRecoveryCodesRegenerated
const TokenPurgeInterval = authn.TokenPurgeInterval
const TokenPurgeJobKind = authn.TokenPurgeJobKind

var AttachResetRequestQueue = authn.AttachResetRequestQueue
var CleanupOtherSessions = authn.CleanupOtherSessions
var CleanupStaleSessions = authn.CleanupStaleSessions
var ClearLoginAttempts = authn.ClearLoginAttempts
var ClearMFAAttempts = authn.ClearMFAAttempts
var ClearReauthAttempts = authn.ClearReauthAttempts
var CurrentPasswordIncorrect = authn.CurrentPasswordIncorrect
var EnsureCanManageMember = authn.EnsureCanManageMember
var LoginLockoutUnavailable = authn.LoginLockoutUnavailable
var NewEmailVerificationService = authn.NewEmailVerificationService
var NewPasswordResetService = authn.NewPasswordResetService
var NewSecurityNotifier = authn.NewSecurityNotifier
var NewSessionService = authn.NewSessionService
var NewTokenPurger = authn.NewTokenPurger
var NewUserService = authn.NewUserService
var ReserveMFAAttempt = authn.ReserveMFAAttempt
var ReserveReauthAttempt = authn.ReserveReauthAttempt

type MFADeps = mfa.MFADeps
type MFAService = mfa.MFAService
type MFAStepUpService = mfa.MFAStepUpService
type PasskeyDeps = mfa.PasskeyDeps
type PasskeyLogin = mfa.PasskeyLogin
type PasskeyService = mfa.PasskeyService

const PasskeyCeremonyTTL = mfa.PasskeyCeremonyTTL

var NewMFAService = mfa.NewMFAService
var NewPasskeyService = mfa.NewPasskeyService

type AdminUserService = membership.AdminUserService
type InvitationService = membership.InvitationService
type InvitationSignUp = membership.InvitationSignUp
type InviteInput = membership.InviteInput
type MemberUpdate = membership.MemberUpdate
type RoleService = membership.RoleService
type UserStats = membership.UserStats

var EnsureCanGrantRoles = membership.EnsureCanGrantRoles
var NewAdminUserService = membership.NewAdminUserService
var NewInvitationService = membership.NewInvitationService
var NewRoleService = membership.NewRoleService
var RemoveMembership = membership.RemoveMembership
var SyncPermissions = membership.SyncPermissions

type PrivacyService = tenancy.PrivacyService
type PurgeTenant = tenancy.PurgeTenant
type TenantPurgeDeps = tenancy.TenantPurgeDeps
type TenantPurgeRepository = tenancy.TenantPurgeRepository
type TenantPurgeService = tenancy.TenantPurgeService
type TenantService = tenancy.TenantService

const TenantDeletionGrace = tenancy.TenantDeletionGrace
const TenantDeletionInterval = tenancy.TenantDeletionInterval
const TenantDeletionJobKind = tenancy.TenantDeletionJobKind
const TenantPurgeInterval = tenancy.TenantPurgeInterval
const TenantPurgeJobKind = tenancy.TenantPurgeJobKind
const TrialExpiryInterval = tenancy.TrialExpiryInterval
const TrialExpiryJobKind = tenancy.TrialExpiryJobKind

var NewTenantPurgeService = tenancy.NewTenantPurgeService
var NewTenantService = tenancy.NewTenantService
var StartupTenants = tenancy.StartupTenants
var TenantStatusTransitionAllowed = tenancy.TenantStatusTransitionAllowed

type ExternalIdentityExport = privacy.ExternalIdentityExport
type IdentityExportRepository = privacy.IdentityExportRepository
type InvitationExport = privacy.InvitationExport
type PasswordVerifier = privacy.PasswordVerifier
type PrivacyDeps = privacy.PrivacyDeps
type RoleAssignmentExport = privacy.RoleAssignmentExport

const DataExportTTL = privacy.DataExportTTL
const ExportPurgeInterval = privacy.ExportPurgeInterval
const JobKindDataExport = privacy.JobKindDataExport
const JobKindErase = privacy.JobKindErase
const JobKindEraseFinalize = privacy.JobKindEraseFinalize
const JobKindExportPurge = privacy.JobKindExportPurge

var NewPrivacyService = privacy.NewPrivacyService

type AuditWriter = core.AuditWriter
type BanHasher = core.BanHasher
type BanMarker = core.BanMarker
type BanMarkerCount = core.BanMarkerCount
type CatalogPermission = core.CatalogPermission
type CatalogRole = core.CatalogRole
type DataExport = core.DataExport
type DataExportRepository = core.DataExportRepository
type DataExportStatus = core.DataExportStatus
type ErasureRequest = core.ErasureRequest
type ErasureScope = core.ErasureScope
type ErasureStatus = core.ErasureStatus
type EventPublisher = core.EventPublisher
type ExternalIdentity = core.ExternalIdentity
type Invitation = core.Invitation
type InvitationRepository = core.InvitationRepository
type JobEnqueuer = core.JobEnqueuer
type Mailer = core.Mailer
type Member = core.Member
type Membership = core.Membership
type MembershipRepository = core.MembershipRepository
type MembershipStatus = core.MembershipStatus
type MemberStats = core.MemberStats
type MFAConfirmResult = core.MFAConfirmResult
type MFAEnrollment = core.MFAEnrollment
type MFAFactor = core.MFAFactor
type MFALogin = core.MFALogin
type MFARepository = core.MFARepository
type MFAStatus = core.MFAStatus
type NewInvitation = core.NewInvitation
type NewRole = core.NewRole
type NewSession = core.NewSession
type NewTenant = core.NewTenant
type NewUser = core.NewUser
type Passkey = core.Passkey
type PasskeyRepository = core.PasskeyRepository
type PasswordResetRepository = core.PasswordResetRepository
type PasswordResetToken = core.PasswordResetToken
type Principal = core.Principal
type ProfileUpdate = core.ProfileUpdate
type RefreshRotationState = core.RefreshRotationState
type RefreshToken = core.RefreshToken
type RefreshTokenRepository = core.RefreshTokenRepository
type RevokeReason = core.RevokeReason
type Role = core.Role
type RoleRepository = core.RoleRepository
type RoleUpdate = core.RoleUpdate
type SecretSealer = core.SecretSealer
type Session = core.Session
type SessionRepository = core.SessionRepository
type SessionState = core.SessionState
type Stores = core.Stores
type Tenant = core.Tenant
type TenantRepository = core.TenantRepository
type TenantSettings = core.TenantSettings
type TenantStatus = core.TenantStatus
type TenantUpdate = core.TenantUpdate
type TokenPair = core.TokenPair
type TokenPurgeRepository = core.TokenPurgeRepository
type Transactor = core.Transactor
type TxRepos = core.TxRepos
type User = core.User
type UserRepository = core.UserRepository
type UserStatus = core.UserStatus
type VerificationRepository = core.VerificationRepository
type VerificationToken = core.VerificationToken

const AMRGoogle = core.AMRGoogle
const AMRHardwareKey = core.AMRHardwareKey
const AMROTP = core.AMROTP
const AMRPassword = core.AMRPassword
const AMRRecoveryCode = core.AMRRecoveryCode
const AMRSoftwareKey = core.AMRSoftwareKey
const DataExportExpired = core.DataExportExpired
const DataExportFailed = core.DataExportFailed
const DataExportQueued = core.DataExportQueued
const DataExportReady = core.DataExportReady
const DataExportRunning = core.DataExportRunning
const ErasureCompleted = core.ErasureCompleted
const ErasureProcessing = core.ErasureProcessing
const ErasureQueued = core.ErasureQueued
const ErasureScopeMember = core.ErasureScopeMember
const ErasureScopeSelf = core.ErasureScopeSelf
const MembershipStatusActive = core.MembershipStatusActive
const MembershipStatusRemoved = core.MembershipStatusRemoved
const MembershipStatusSuspended = core.MembershipStatusSuspended
const MFAFactorTOTP = core.MFAFactorTOTP
const PasskeyMethodHardware = core.PasskeyMethodHardware
const PasskeyMethodSoftware = core.PasskeyMethodSoftware
const ProviderGoogle = core.ProviderGoogle
const RevokeAccountDeleted = core.RevokeAccountDeleted
const RevokeByUser = core.RevokeByUser
const RevokeCredentialsChanged = core.RevokeCredentialsChanged
const RevokeLogout = core.RevokeLogout
const RevokeLogoutAll = core.RevokeLogoutAll
const RevokeMembershipChanged = core.RevokeMembershipChanged
const RevokeMFAChanged = core.RevokeMFAChanged
const RevokeRefreshReuse = core.RevokeRefreshReuse
const RoleAdmin = core.RoleAdmin
const RoleMember = core.RoleMember
const RoleOwner = core.RoleOwner
const TenantStatusActive = core.TenantStatusActive
const TenantStatusDeleted = core.TenantStatusDeleted
const TenantStatusPendingDeletion = core.TenantStatusPendingDeletion
const TenantStatusSuspended = core.TenantStatusSuspended
const TenantStatusTrialing = core.TenantStatusTrialing
const UserStatusActive = core.UserStatusActive
const UserStatusDeleted = core.UserStatusDeleted
const UserStatusSuspended = core.UserStatusSuspended

var ErrAlreadyMember = core.ErrAlreadyMember
var ErrDataExportInProgress = core.ErrDataExportInProgress
var ErrEmailTaken = core.ErrEmailTaken
var ErrIdentityConflict = core.ErrIdentityConflict
var ErrIdentityTaken = core.ErrIdentityTaken
var ErrPasskeyAlreadyRegistered = core.ErrPasskeyAlreadyRegistered
var ErrRoleInUse = core.ErrRoleInUse
var ErrRoleKeyTaken = core.ErrRoleKeyTaken
var ErrRoleNotFound = core.ErrRoleNotFound
var ErrTenantSlugTaken = core.ErrTenantSlugTaken
var AccountNotActive = core.AccountNotActive
var AuditWriteError = core.AuditWriteError
var BuildCursor = core.BuildCursor
var CheckAudit = core.CheckAudit
var ClientIPFromContext = core.ClientIPFromContext
var CreateAccount = core.CreateAccount
var EmailKey = core.EmailKey
var EmailTaken = core.EmailTaken
var EmailUnavailable = core.EmailUnavailable
var EnsureEmailNotBanned = core.EnsureEmailNotBanned
var EnsureNotLastOwner = core.EnsureNotLastOwner
var HashPassword = core.HashPassword
var HashToken = core.HashToken
var InvalidateSessions = core.InvalidateSessions
var MemberPage = core.MemberPage
var NewBanMarker = core.NewBanMarker
var NewOpaqueToken = core.NewOpaqueToken
var NormalizeEmail = core.NormalizeEmail
var RemovalBanMarker = core.RemovalBanMarker
var SpendDummyPasswordWork = core.SpendDummyPasswordWork
var TokenLink = core.TokenLink
var Truncate = core.Truncate
var UserAgentFromContext = core.UserAgentFromContext
var UserEvent = core.UserEvent
var UserWriteError = core.UserWriteError
var ValidatePasswordForEmail = core.ValidatePasswordForEmail
var ValidMembershipStatus = core.ValidMembershipStatus
var ValidRole = core.ValidRole
var VerifyPassword = core.VerifyPassword
var VerifyPasswordHash = core.VerifyPasswordHash
var WithClientIP = core.WithClientIP
var WithLinkTenantSlug = core.WithLinkTenantSlug
var WithUserAgent = core.WithUserAgent

type ExportJob = privacy.ExportJob

type ExportDocument = privacy.ExportDocument

type EraseJob = privacy.EraseJob

type FinalizeJob = privacy.FinalizeJob

var ErrErasurePending = privacy.ErrErasurePending

var NewPrivacyServiceWithRegistry = privacy.NewPrivacyServiceWithRegistry
