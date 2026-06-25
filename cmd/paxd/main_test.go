package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppExposesOnlyDaemonAndServiceCommands(t *testing.T) {
	app := newApp()

	names := make([]string, 0, len(app.Commands))
	for _, command := range app.Commands {
		names = append(names, command.Name)
	}

	assert.ElementsMatch(t, []string{"run", "service"}, names)
	assert.NotContains(t, names, "connect")
	assert.NotContains(t, names, "configure")
	assert.NotContains(t, names, "register")
	assert.NotContains(t, names, "acp-forward")
	assert.NotContains(t, names, "postman")
	assert.NotContains(t, names, "harnesses")
	assert.NotContains(t, names, "install-service")
}
