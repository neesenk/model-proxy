package admin

import (
	"context"
	"errors"
	"fmt"
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
	// accountID, when non-empty, is a TARGETED re-login: the completed SSO
	// flow must re-authorize exactly this pooled account (same upstream
	// identity); landing on a different account is surfaced as an error
	// instead of silently adding a sibling.
	accountID string
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
	seed, err := login.AqpOAuthSeed(account)
	if err != nil {
		return fail(err)
	}
	id, err := login.AddOAuthAccount(job.name, "aqp", seed)
	if err != nil {
		return fail(err)
	}
	if job.accountID != "" && id != job.accountID {
		return fail(fmt.Errorf("re-login authorized a different account (id %s); the original account is unchanged", maskAccountID(id)))
	}
	return appapi.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  account.Email,
		Warning: job.service.reloadAfterMutation(),
	}
}

// maskAccountID keeps login errors free of full identifiers while still
// letting the operator tell two accounts apart (first 2 + … + last 2).
func maskAccountID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[:2] + "…" + id[len(id)-2:]
}

type codexLoginJob struct {
	service *Service
	name    string
	state   codexLoginState
	detail  string
	// accountID is the targeted re-login contract (see aqpLoginJob).
	accountID string
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
	seed, err := login.CodexOAuthSeed(authFile)
	if err != nil {
		return fail(err)
	}
	id, err := login.AddOAuthAccount(job.name, "codex", seed)
	if err != nil {
		return fail(err)
	}
	if job.accountID != "" && id != job.accountID {
		return fail(fmt.Errorf("re-login authorized a different account (id %s); the original account is unchanged", maskAccountID(id)))
	}
	result := seed.Email
	if result == "" {
		result = seed.UpstreamID
	}
	return appapi.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  result,
		Warning: job.service.reloadAfterMutation(),
	}
}

func (s *Service) BeginLogin(
	ctx context.Context,
	name string,
	accountID string,
) (appapi.LoginStart, error) {
	providerConfig, ok := s.ports.ProviderConfig(name)
	if !ok {
		return appapi.LoginStart{}, appapi.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	// A targeted re-login must name an existing pooled account — fail fast
	// instead of running a whole SSO/device flow that cannot land on it.
	if accountID != "" {
		snapshot, _ := accounts.NewStore(accounts.HomeDir()).LoadSnapshot(name, providerConfig.Provider)
		found := false
		for _, a := range snapshot.Pool.Accounts {
			if a.ID == accountID {
				found = true
				break
			}
		}
		if !found {
			return appapi.LoginStart{}, appapi.NewHTTPError(
				http.StatusBadRequest,
				"unknown account: "+accountID,
			)
		}
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
				service:   s,
				name:      name,
				client:    client,
				detail:    loginURL,
				accountID: accountID,
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
				detail:    detail,
				accountID: accountID,
			},
		}, nil
	default:
		return appapi.LoginStart{}, appapi.NewHTTPError(
			http.StatusBadRequest,
			name+" has no async login flow",
		)
	}
}
