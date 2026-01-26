package reverse

//
// @Author yfy2001
// @Date 2026/1/13 14 26
//

type ReverseType string

const (
	ReverseCmd     ReverseType = "cmd"
	ReverseCompose ReverseType = "compose"
	ReverseAll     ReverseType = "all"
)

type ReverseOptions struct {
	PreserveVolumes   bool        // 保留匿名卷名字
	FilterDefaultEnvs bool        // 过滤掉 Docker 默认环境变量
	PrettyFormat      bool        // 格式化输出 docker run 命令
	MergePorts        bool        // 合并连续端口范围
	Rerun             bool        // 是否重新运行容器
	Save              bool        // 是否保存输出到文件
	ReverseType       ReverseType // 输出类型: cmd | compose | all
}
