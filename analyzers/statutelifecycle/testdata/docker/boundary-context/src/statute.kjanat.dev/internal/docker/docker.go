package docker

import "context"

type Client struct{}

func (*Client) StartContainer(context.Context, string) error { return nil }
func (*Client) StopContainer(context.Context, string) error  { return nil }
