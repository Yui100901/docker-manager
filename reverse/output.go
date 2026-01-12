package reverse

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//
// @Author yfy2001
// @Date 2026/1/12 22 25
//

type ReverseType string

const (
	ReverseCmd     ReverseType = "cmd"
	ReverseCompose ReverseType = "compose"
	ReverseAll     ReverseType = "all"
)

func buildOutput(results []ParsedResult, rt ReverseType) (map[string]string, map[string]ComposeService) {
	cmdMap := make(map[string]string)
	composeMap := make(map[string]ComposeService)

	for _, r := range results {
		if rt == ReverseCmd || rt == ReverseAll {
			cmdMap[r.Name] = strings.Join(r.Command, " ")
		}
		if rt == ReverseCompose || rt == ReverseAll {
			composeMap[r.Name] = r.Compose
		}
	}

	return cmdMap, composeMap
}

func printOutput(cmdMap map[string]string, composeMap map[string]ComposeService, rt ReverseType) {
	if rt == ReverseCmd || rt == ReverseAll {
		for name, cmd := range cmdMap {
			fmt.Printf("# %s\n%s\n\n", name, cmd)
		}
	}

	if rt == ReverseCompose || rt == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: composeMap})
		fmt.Println(string(yml))
	}
}
