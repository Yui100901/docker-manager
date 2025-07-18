# docker-manager
A simple cmd util to manager docker.

简单的docker小工具用于快捷完成一些docker操作

编译：go build -o dm .

使用介绍dm -h

```
Usage:
  dm <command> [flags]
  dm [command]

Available Commands:
  build       构建Docker镜像，默认为当前目录
  clean       清理Docker镜像
  completion  Generate the autocompletion script for the specified shell
  export      导出Docker镜像，默认为当前路径下的images
  help        Help about any command
  import      导入Docker镜像，默认从images，以及所有子目录寻找镜像
  pull        无需docker客户端，下载docker镜像
  reverse     逆向Docker容器到启动命令

Flags:
  -h, --help   help for dm

Use "dm [command] --help" for more information about a command.
```