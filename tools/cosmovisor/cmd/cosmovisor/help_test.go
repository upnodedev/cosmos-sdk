package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/upnodedev/cosmos-sdk/tools/cosmovisor"
)

func TestGetHelpText(t *testing.T) {
	expectedPieces := []string{
		"Cosmovisor",
		cosmovisor.EnvName, cosmovisor.EnvHome,
		"https://docs.cosmos.network/main/tooling/cosmovisor",
	}

	actual := GetHelpText()
	for _, piece := range expectedPieces {
		assert.Contains(t, actual, piece)
	}
}
