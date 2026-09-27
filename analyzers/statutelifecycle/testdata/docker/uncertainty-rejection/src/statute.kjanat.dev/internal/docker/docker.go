package docker

import "context"

type Client struct{}

type Inspection struct{ Running bool }

func (*Client) InspectContainer(context.Context, string) (Inspection, error) {
	return Inspection{}, nil
}
func LifecycleContainerMissing(error) bool { return false }
func LifecycleOutcomeAmbiguous(error) bool { return true }

func (*Client) StartContainer(context.Context, string) error { return nil }
func (*Client) StopContainer(context.Context, string) error  { return nil }
