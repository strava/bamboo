package configuration

import (
	"strings"
)

/*
	Mesos Marathon configuration
*/
type Marathon struct {
	// comma separated marathon http endpoints including port number
	Endpoint string

	UseEventStream bool
}

func (m Marathon) Endpoints() []string {
	return strings.Split(m.Endpoint, ",")
}
