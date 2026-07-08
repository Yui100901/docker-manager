package targets

import (
	"fmt"
	"strings"
)

type ContainerSelection struct {
	Count      int      `json:"count"`
	DefaultAll bool     `json:"default_all"`
	Running    bool     `json:"running"`
	Filters    []string `json:"filters,omitempty"`
	Message    string   `json:"message"`
}

func BuildContainerSelection(action string, count int, running bool, filters []string) ContainerSelection {
	target := ContainerSelection{
		Count:      count,
		DefaultAll: len(filters) == 0 && !running,
		Running:    running,
		Filters:    append([]string(nil), filters...),
	}
	switch {
	case target.DefaultAll:
		target.Message = fmt.Sprintf("未指定容器筛选，默认%s全部本地容器 %d 个", action, count)
	case running && len(filters) == 0:
		target.Message = fmt.Sprintf("仅%s运行中容器 %d 个", action, count)
	case running:
		target.Message = fmt.Sprintf("在运行中容器内按筛选条件 %q 选中 %d 个", strings.Join(filters, ", "), count)
	default:
		target.Message = fmt.Sprintf("按筛选条件 %q 选中 %d 个容器", strings.Join(filters, ", "), count)
	}
	return target
}
