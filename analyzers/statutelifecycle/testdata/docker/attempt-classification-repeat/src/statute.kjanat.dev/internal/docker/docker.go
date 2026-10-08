package docker

import "context"

type (
	Client     struct{}
	Inspection struct{ Running bool }
)

func (*Client) StopContainer(context.Context, string) error { return nil }
func (*Client) InspectContainer(context.Context, string) (Inspection, error) {
	return Inspection{}, nil
}
func LifecycleContainerMissing(error) bool { return false }
func LifecycleOutcomeAmbiguous(error) bool { return true }
