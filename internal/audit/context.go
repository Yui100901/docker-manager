package audit

import "context"

type sessionContextKey struct{}

func WithSession(ctx context.Context, session *Session) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func FromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	session, _ := ctx.Value(sessionContextKey{}).(*Session)
	return session
}
