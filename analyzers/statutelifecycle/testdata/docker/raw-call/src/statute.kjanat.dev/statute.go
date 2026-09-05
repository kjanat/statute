package statute

import (
	"context"

	"statute.kjanat.dev/internal/docker"
)

func rawMutations(client *docker.Client, ctx context.Context) {
	_ = client.StartContainer(ctx, "mutable-name") // want `\[SLC105\].*StartContainer may only be called directly from.*startActivation`
	_ = client.StopContainer(ctx, "mutable-name")  // want `\[SLC105\].*StopContainer may only be called directly from.*attemptOwnedStop`
}

func escapedMutation(client *docker.Client) {
	start := client.StartContainer // want `\[SLC105\].*StartContainer reference escapes`
	_ = start
}

type mutationClient interface {
	StopContainer(context.Context, string) error
}

func interfaceMutation(client mutationClient, ctx context.Context) {
	_ = client.StopContainer(ctx, "mutable-name") // want `\[SLC105\].*interface dispatch cannot prove`
}

type fakeClient struct{}

func (*fakeClient) StartContainer(context.Context, string) error { return nil }
func (*fakeClient) StopContainer(context.Context, string) error  { return nil }

func sameNamesAreIgnored(client *fakeClient, ctx context.Context) {
	_ = client.StartContainer(ctx, "name")
	_ = client.StopContainer(ctx, "name")
}
