package cli

import "context"

func reportUpCmd(ctx context.Context, args []string, env Env) int {
	return usageErr(env, "report-up", "not built yet")
}
func reportsCmd(ctx context.Context, args []string, env Env) int {
	return usageErr(env, "reports", "not built yet")
}
func reportAckCmd(ctx context.Context, args []string, env Env) int {
	return usageErr(env, "report-ack", "not built yet")
}
func reportDeliveredCmd(ctx context.Context, args []string, env Env) int {
	return usageErr(env, "report-delivered", "not built yet")
}
