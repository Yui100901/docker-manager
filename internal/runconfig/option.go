package runconfig

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
	Save              bool        // 是否保存输出到文件
	ReverseType       ReverseType // 输出类型: cmd | compose | all
	RedactSecrets     bool        // 是否脱敏 env/label 中疑似敏感字段
}
