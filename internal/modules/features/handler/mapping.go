package handler

import (
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuresservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
)

func toFlagResponse(flag featuresservice.Flag) featuresdto.FeatureFlagResponse {
	return featuresdto.FeatureFlagResponse{
		Key:         flag.Key,
		Description: flag.Description,
		Enabled:     flag.Enabled,
		KillSwitch:  flag.KillSwitch,
		Configured:  flag.Configured,
		Effective:   flag.Effective,
		Source:      flag.Source,
		CreatedAt:   flag.CreatedAt,
		UpdatedAt:   flag.UpdatedAt,
	}
}

func toFlagResponses(flags []featuresservice.Flag) []featuresdto.FeatureFlagResponse {
	out := make([]featuresdto.FeatureFlagResponse, len(flags))
	for i := range flags {
		out[i] = toFlagResponse(flags[i])
	}
	return out
}

func toEvaluatedFlagResponses(flags []featuresservice.EvaluatedFlag) []featuresdto.EvaluatedFlagResponse {
	out := make([]featuresdto.EvaluatedFlagResponse, len(flags))
	for i := range flags {
		out[i] = featuresdto.EvaluatedFlagResponse{
			Key:        flags[i].Key,
			Enabled:    flags[i].Enabled,
			KillSwitch: flags[i].KillSwitch,
			Source:     flags[i].Source,
		}
	}
	return out
}

// toPlatformTargetResponses renders cross-tenant item ids: tenant:{tenantId}
// and user:{tenantId}:{userId}.
func toPlatformTargetResponses(targets []featuresservice.Target) []featuresdto.FeatureFlagTargetResponse {
	out := make([]featuresdto.FeatureFlagTargetResponse, len(targets))
	for i := range targets {
		out[i] = toPlatformTargetResponse(targets[i])
	}
	return out
}

func toPlatformTargetResponse(target featuresservice.Target) featuresdto.FeatureFlagTargetResponse {
	itemID := "tenant:" + target.TenantID.String()
	if target.UserID != nil {
		itemID = "user:" + target.TenantID.String() + ":" + target.UserID.String()
	}
	return targetResponse(target, itemID)
}

// toTenantTargetResponses renders item ids relative to the request tenant.
func toTenantTargetResponses(targets []featuresservice.Target) []featuresdto.FeatureFlagTargetResponse {
	out := make([]featuresdto.FeatureFlagTargetResponse, len(targets))
	for i := range targets {
		out[i] = toTenantTargetResponse(targets[i])
	}
	return out
}

func toTenantTargetResponse(target featuresservice.Target) featuresdto.FeatureFlagTargetResponse {
	itemID := "tenant"
	if target.UserID != nil {
		itemID = "user:" + target.UserID.String()
	}
	return targetResponse(target, itemID)
}

func targetResponse(target featuresservice.Target, itemID string) featuresdto.FeatureFlagTargetResponse {
	return featuresdto.FeatureFlagTargetResponse{
		TargetID:  itemID,
		FlagKey:   target.FlagKey,
		TenantID:  target.TenantID,
		UserID:    target.UserID,
		Enabled:   target.Enabled,
		CreatedAt: target.CreatedAt,
		UpdatedAt: target.UpdatedAt,
	}
}
