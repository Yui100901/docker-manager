package backup

import "context"

func backupContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func checkBackupContext(ctx context.Context) error {
	return backupContext(ctx).Err()
}
