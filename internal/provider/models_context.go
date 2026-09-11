package provider

import "context"

// FetchModelsContext is the cancellable model-list capability used by daemon
// refreshes. Legacy implementations without this capability are unsupported;
// never detach an uncancellable FetchModels call into an unowned goroutine.
func FetchModelsContext(ctx context.Context, impl Provider) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fetcher, ok := impl.(interface {
		FetchModelsContext(context.Context) ([]string, error)
	}); ok {
		return fetcher.FetchModelsContext(ctx)
	}
	return nil, errNotSupported
}

func (p *AqpProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
func (p *ZhipuProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
func (p *ZCodeProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
func (p *DeepSeekProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
func (p *KimiCodeProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
func (p *QwenPlanProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	return fetchModelsBearerContext(ctx, p.cfg, p.AuthHeaders)
}
