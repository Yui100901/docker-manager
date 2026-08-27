package runcontrol

import "context"

// CheckItems reserves count items from the controller attached to ctx. It is
// intentionally a no-op when the command has no runtime controller so service
// functions remain usable in isolation and keep their historical defaults.
func CheckItems(ctx context.Context, kind string, count int) error {
	if ctx == nil {
		return nil
	}
	controller, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return controller.CheckItems(kind, count)
}
