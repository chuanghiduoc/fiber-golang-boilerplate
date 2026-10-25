package handler

import (
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
)

func toFlagResponse(flag featuressvc.Flag) featuresdto.FlagResponse {
	return featuresdto.FlagResponse{
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

func toFlagResponses(flags []featuressvc.Flag) []featuresdto.FlagResponse {
	out := make([]featuresdto.FlagResponse, len(flags))
	for i := range flags {
		out[i] = toFlagResponse(flags[i])
	}
	return out
}

func toFlagDetailResponse(detail *featuressvc.FlagDetail) featuresdto.FlagDetailResponse {
	return featuresdto.FlagDetailResponse{
		Flag:    toFlagResponse(detail.Flag),
		Targets: toTargetResponses(detail.Targets),
	}
}

func toTargetResponse(target featuressvc.Target) featuresdto.TargetResponse {
	return featuresdto.TargetResponse{
		ID:        target.ID,
		FlagKey:   target.FlagKey,
		TenantID:  target.TenantID,
		UserID:    target.UserID,
		Enabled:   target.Enabled,
		CreatedAt: target.CreatedAt,
		UpdatedAt: target.UpdatedAt,
	}
}

func toTargetResponses(targets []featuressvc.Target) []featuresdto.TargetResponse {
	out := make([]featuresdto.TargetResponse, len(targets))
	for i := range targets {
		out[i] = toTargetResponse(targets[i])
	}
	return out
}

func toUpsertFlagInput(req featuresdto.UpsertFlagRequest) featuressvc.UpsertFlagInput {
	return featuressvc.UpsertFlagInput{
		Enabled:     req.Enabled,
		KillSwitch:  req.KillSwitch,
		Description: req.Description,
	}
}

func toUpsertTargetInput(req featuresdto.UpsertTargetRequest) featuressvc.UpsertTargetInput {
	return featuressvc.UpsertTargetInput{
		UserID:  req.UserID,
		Enabled: req.Enabled,
	}
}
