package reverse

import (
	"docker-manager/internal/runconfig"

	"github.com/moby/moby/api/types/container"
)

type ReverseType = runconfig.ReverseType

const (
	ReverseCmd     = runconfig.ReverseCmd
	ReverseCompose = runconfig.ReverseCompose
	ReverseAll     = runconfig.ReverseAll
)

type ReverseOptions = runconfig.ReverseOptions
type ContainerSpec = runconfig.ContainerSpec
type PortBindingSpec = runconfig.PortBindingSpec
type UlimitSpec = runconfig.UlimitSpec
type ParsedResult = runconfig.ParsedResult
type ComposeFile = runconfig.ComposeFile
type ComposeService = runconfig.ComposeService
type ComposeLogging = runconfig.ComposeLogging
type CommandFormatter = runconfig.CommandFormatter
type ComposeFormatter = runconfig.ComposeFormatter
type Parser = runconfig.Parser

const CommandSplitMarker = runconfig.CommandSplitMarker

func NewParser(ci container.InspectResponse, opts ReverseOptions) *Parser {
	return runconfig.NewParser(ci, opts)
}

func mergePortRanges(bindings []PortBindingSpec) []string {
	return runconfig.MergePortRanges(bindings)
}
