package main

import (
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/Traviis/nomad-driver-kata/kata"
)

func main() {
	plugins.Serve(factory)
}

func factory(logger log.Logger) interface{} {
	return kata.NewDriver(logger)
}
