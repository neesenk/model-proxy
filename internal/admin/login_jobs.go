package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	"model-proxy/internal/login"
	"model-proxy/internal/provider"
)

type aqpLoginJob struct {
	service *Service
	name    string
	client  *login.AqpClient
	detail  string
}

func (job *aqpLoginJob) Run(ctx context.Context) appapi.LoginUpdate {
	fail := func(err error) appapi.LoginUpdate {
		return appapi.LoginUpdate{
			State:  "error",
			Detail: job.detail,
			Result: err.Error(),
		}
	}
	if _, err := job.client.PollSessionContext(ctx, 3*time.Minute); err != nil {
		return fail(err)
	}
	keyData, err := job.client.FetchAPIKeyContext(ctx)
	if err != nil {
		return fail(errors.New("api key provisioning: " + err.Error()))
	}
	account := &provider.AqpAccountData{
		AccountID:        keyData.EmployeeEmail,
		Email:            keyData.EmployeeEmail,
		ProjectID:        keyData.ProjectID,
		SSOSessionCookie: job.client.SessionCookie(),
		LastRefreshAt:    time.Now().Unix(),
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := provider.SaveAqpAccount(
		accounts.AuthFilePath(job.name, "oauth_auth"),
		account,
	); err != nil {
		return fail(err)
	}
	return appapi.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  account.Email,
		Warning: job.service.reloadAfterMutation(),
	}
}

type codexLoginJob struct {
	service *Service
	name    string
	state   codexLoginState
	detail  string
}

// codexLoginState is application-owned device-flow state. The Web transport
// retains only the resulting appapi.LoginUpdate and never sees OAuth client details.
type codexLoginState struct {
	deviceAuthID string
	userCode     string
	interval     int
	opts         *login.CodexLoginServerOptions
}

func (job *codexLoginJob) Run(ctx context.Context) appapi.LoginUpdate {
	fail := func(err error) appapi.LoginUpdate {
		return appapi.LoginUpdate{
			State:  "error",
			Detail: job.detail,
			Result: err.Error(),
		}
	}
	authCode, err := login.PollForTokenContext(
		ctx,
		job.state.opts,
		job.state.deviceAuthID,
		job.state.userCode,
		job.state.interval,
	)
	if err != nil {
		return fail(err)
	}
	authFile, err := login.ExchangeCodeForTokensContext(
		ctx,
		job.state.opts,
		provider.CodexOAuthClientID,
		authCode.AuthorizationCode,
		authCode.CodeVerifier,
	)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := provider.WriteCodexAuthFile(
		accounts.AuthFilePath(job.name, "oauth_auth"),
		authFile,
	); err != nil {
		return fail(err)
	}
	return appapi.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  authFile.Tokens.AccountID,
		Warning: job.service.reloadAfterMutation(),
	}
}

func (s *Service) BeginLogin(
	ctx context.Context,
	name string,
) (appapi.LoginStart, error) {
	providerConfig, ok := s.ports.ProviderConfig(name)
	if !ok {
		return appapi.LoginStart{}, appapi.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp":
		client := s.ports.NewAqpClient(accounts.AuthFilePath(name, "oauth_auth"))
		loginURL, err := client.BootstrapLoginURLContext(ctx)
		if err != nil {
			return appapi.LoginStart{}, appapi.NewHTTPError(
				http.StatusBadGateway,
				err.Error(),
			)
		}
		return appapi.LoginStart{
			Provider: "aqp",
			LoginURL: loginURL,
			Job: &aqpLoginJob{
				service: s,
				name:    name,
				client:  client,
				detail:  loginURL,
			},
		}, nil
	case "codex":
		options := s.ports.NewCodexOptions()
		userCode, err := login.RequestUserCodeContext(
			ctx,
			options,
			provider.CodexOAuthClientID,
		)
		if err != nil {
			return appapi.LoginStart{}, appapi.NewHTTPError(
				http.StatusBadGateway,
				err.Error(),
			)
		}
		intervalSeconds, _ := strconv.Atoi(userCode.Interval)
		detail := login.CodexOAuthVerifyURL + "  code: " + userCode.UserCode
		return appapi.LoginStart{
			Provider:  "codex",
			VerifyURL: login.CodexOAuthVerifyURL,
			UserCode:  userCode.UserCode,
			Job: &codexLoginJob{
				service: s,
				name:    name,
				state: codexLoginState{
					deviceAuthID: userCode.DeviceAuthID,
					userCode:     userCode.UserCode,
					interval:     intervalSeconds,
					opts:         options,
				},
				detail: detail,
			},
		}, nil
	default:
		return appapi.LoginStart{}, appapi.NewHTTPError(
			http.StatusBadRequest,
			name+" has no async login flow",
		)
	}
}
