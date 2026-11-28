// BRP 管线（M0 第 10 天骨架；day40 周整理补课：三端 lint+单测进管线——实施规划 §9/§11 既有要求）。
// M6 完整化时在此追加 docker build / compose 部署阶段（实施规划 §11）。
// 已知边界（记档）：
// - Go 集成测试在 Jenkins 静默跳过（无 PG/Redis；测试代码缺 BRP_TEST_DSN 即 skip）——单测仍全量；
// - agent 依赖经 uv export 哈希清单 + pypi.org 安装（清华镜像 403，不能按锁内地址下载，
//   也不提交换源重锁——AGENTS.md §9）；venv 放 workspace 跨构建复用（pip 按哈希清单
//   幂等安装、版本精确锁定——首次触发实测全绿，2026-11-27）。
pipeline {
    agent any
    options {
        timestamps()
        disableConcurrentBuilds()
    }
    environment {
        // Jenkins 服务进程的 PATH/GOENV 与登录用户不同：工具链显式注入
        // Go 在 F:\Go\bin；golangci-lint 在用户 go/bin；uv 在 ~\.local\bin；
        // python/ruff 在 "D:\Python packages"（含 Scripts）；node/npm 在 D:\nodejs
        PATH = "${env.PATH};F:\\Go\\bin;C:\\Users\\yangj\\go\\bin;C:\\Users\\yangj\\.local\\bin;D:\\Python packages;D:\\Python packages\\Scripts;D:\\nodejs"
        GOPROXY = 'https://goproxy.cn,direct'
    }
    stages {
        stage('checkout') {
            steps {
                checkout scm
                bat 'go version' // 环境探针：PATH 注入是否生效第一时间可见
            }
        }
        stage('backend: golangci-lint') {
            steps {
                dir('backend') {
                    bat 'golangci-lint run ./...'
                }
            }
        }
        stage('backend: go test') {
            steps {
                dir('backend') {
                    bat 'go test ./...'
                }
            }
        }
        stage('agent: ruff') {
            steps {
                dir('agent') {
                    bat 'ruff check .'
                }
            }
        }
        stage('agent: pytest') {
            steps {
                // 离线导出锁内精确版本+哈希清单（含 dev 组：pytest/jsonschema），经 pypi.org 安装进一次性 venv
                bat 'uv export --project agent --frozen --offline --no-emit-project --no-header -o agent-req.txt'
                bat 'python -m venv agent-venv'
                bat 'agent-venv\\Scripts\\python -m pip install -q -i https://pypi.org/simple --require-hashes -r agent-req.txt'
                dir('agent') {
                    bat '..\\agent-venv\\Scripts\\python -m pytest tests -q'
                }
            }
        }
        stage('frontend: eslint') {
            steps {
                dir('frontend') {
                    bat 'npm ci --no-audit --no-fund'
                    bat 'npm run lint'
                }
            }
        }
    }
    post {
        failure {
            echo "BRP 管线失败：查看上方各阶段日志定位（常见：工具链 PATH、模块下载代理、npm 源可达性）"
        }
    }
}
