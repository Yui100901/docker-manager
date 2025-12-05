# docker-manager
A simple cmd util to manager docker.

简单的docker小工具用于快捷完成一些docker操作

编译：go build -o dm .

使用介绍dm -h

```
Docker小工具，可用于管理容器.
Author:Yui

Usage:
  dm <command> [flags]
  dm [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  load        导入Docker镜像，默认从images，以及所有子目录寻找镜像
  pull        无需docker客户端，下载docker镜像
  reverse     逆向Docker容器到启动命令
  save        导出Docker镜像，默认为当前路径下的images。

Flags:
  -h, --help   help for dm

Use "dm [command] --help" for more information about a command.
```