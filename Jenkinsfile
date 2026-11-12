// BRP 骨架管线（M0 第 10 天）：checkout → golangci-lint → go test。
// M6 完整化时在此追加 docker build / compose 部署阶段（实施规划 §11）。
// 范围：仅 backend（前端/agent 的测试基建后续里程碑接入）。
pipeline {
    agent any
    options {
        timestamps()
        disableConcurrentBuilds()
    }
    environment {
        // Jenkins 服务进程的 PATH/GOENV 与登录用户不同：
        // Go 在 F:\Go\bin（系统 PATH），golangci-lint 在用户 go/bin（服务不可见）——显式注入；
        // go env -w 的 GOPROXY 按用户存，服务账户读不到——用环境变量覆盖。
        PATH = "${env.PATH};F:\\Go\\bin;C:\\Users\\yangj\\go\\bin"
        GOPROXY = 'https://goproxy.cn,direct'
    }
    stages {
        stage('checkout') {
            steps {
                checkout scm
                bat 'go version' // 环境探针：PATH 注入是否生效第一时间可见
            }
        }
        stage('golangci-lint') {
            steps {
                dir('backend') {
                    bat 'golangci-lint run ./...'
                }
            }
        }
        stage('go test') {
            steps {
                dir('backend') {
                    bat 'go test ./...'
                }
            }
        }
    }
    post {
        failure {
            echo "BRP 管线失败：查看上方各阶段日志定位（常见：工具链 PATH、模块下载代理）"
        }
    }
}
