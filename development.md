# ChatClaw

## 前置依赖

### npm

```
nvm install --lts
```

### Wails3 cli

```shell
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
```

### Windows CGO 环境配置（UCRT64）

本项目使用 CGO 版本的 sqlite-vec 扩展，需要配置 C 编译环境。

#### 1. 安装 MSYS2

从 [https://www.msys2.org/](https://www.msys2.org/) 下载并安装 MSYS2。

#### 2. 安装 GCC 和 SQLite3 开发库

打开 **MSYS2 UCRT64** 终端，执行：

```bash
# 安装 GCC 编译器
pacman -S mingw-w64-ucrt-x86_64-gcc

# 安装 SQLite3 开发库（包含 sqlite3.h 头文件）
pacman -S mingw-w64-ucrt-x86_64-sqlite3
```

#### 3. 配置 PATH 环境变量

将 MSYS2 UCRT64 的 bin 目录添加到系统 PATH：

```
C:\msys64\ucrt64\bin
```

#### 4. 验证安装

```bash
gcc --version
# 应输出类似: gcc.exe (Rev8, Built by MSYS2 project) 15.x.x
```

#### 5. 构建项目

CGO 已在 `build/windows/Taskfile.yml` 中默认启用（`CGO_ENABLED=1`）。

---

#### Windows 安装包依赖（makensis）

Windows 打包（生成安装包）需要安装 **makensis（NSIS）**。

- 参考文档：`https://wails.io/zh-Hans/docs/next/guides/windows-installer/`
- 安装后将 makensis 安装目录添加到 **Path** 环境变量中（确保命令行可直接执行 `makensis`）

## 开发

```bash
# 安装 openclaw
go run ./internal/tools/openclawbundle -config build/runtime.yml

# gui模式
wails3 dev

# server模式 (only linux)
wails3 task build:server
wails3 task run:server
```


## 额外技能包
将额外的技能skill打包成extraSkills.zip放到，build\extraSkills\extraSkills.zip，方便打包

## Windows 打包

```bash
# amd64
wails3 task windows:build ARCH=amd64 DEV=false
cd bin && 7z a ChatClaw_windows_amd64.zip ChatClaw.exe && cd ..
wails3 task windows:package ARCH=amd64 DEV=false
# 包含 openclaw的包 最好自己弄zip压缩包 build\openclaw-runtime\windows-amd64内的文件 压缩到build\openclaw-runtime\windows-amd64.zip中，方便直接导出
wails3 task windows:package ARCH=amd64 DEV=false BUNDLE_OPENCLAW=true
```

## macos 多架构打包

```bash
# arm64
wails3 task darwin:sign:notarize ARCH=arm64 DEV=false
cd bin && tar -czf ChatClaw_darwin_arm64.tar.gz -C ChatClaw.app/Contents/MacOS ChatClaw && mv ChatClaw-arm64.dmg ./ChatClaw_AppleCPU_arm64.dmg && cd ..

# amd64
wails3 task darwin:sign:notarize ARCH=amd64 DEV=false
cd bin && tar -czf ChatClaw_darwin_amd64.tar.gz -C ChatClaw.app/Contents/MacOS ChatClaw &&  mv ChatClaw-amd64.dmg ./ChatClaw_IntelCPU_amd64.dmg && cd ..

# arm64+amd64
wails3 task darwin:sign:notarize UNIVERSAL=true DEV=false
cd bin && mv ChatClaw-universal.dmg ./ChatClaw_MacOS_universal.dmg && cd ..
```

## Linux Server 模式构建、打包

```bash
docker login registry.cn-hangzhou.aliyuncs.com
wails3 generate bindings -clean -ts && cd frontend && npm i && npm run build && cd ..
wails3 task build:docker PLATFORM=multi  (wails3 task build:docker PLATFORM=amd64)
mv ./bin/linux_amd64/server ./bin/ChatClaw_server_linux_amd64
mv ./bin/linux_arm64/server ./bin/ChatClaw_server_linux_arm64

# 单独导出 OpenClaw runtime 压缩包
wails3 task bundle:openclaw:runtime PLATFORM=multi  (wails3 task bundle:openclaw:runtime PLATFORM=amd64)
# 输出:
# build/openclaw-runtime/linux-amd64.tar.gz
# build/openclaw-runtime/linux-arm64.tar.gz
```



---

## 自动化测试

### 前端测试环境配置

#### 1. 安装 pnpm

```bash
npm install -g pnpm
```

#### 2. 安装前端依赖和 Playwright

```bash
cd frontend
pnpm install
pnpm playwright install chromium
```

#### 3. 构建后端（WebView2 测试需要）

```bash
# 在项目根目录执行
go build -o bin/ChatClaw.exe .
```

### 运行 E2E 测试

> WebView2 模式直接测试真实应用，可验证 Go 后端与前端 Vue 的完整交互。

```bash
cd frontend
pnpm playwright test --config playwright.webview2.config.ts
```

### 测试文件位置

```
chatclaw/
├── internal/**/*_test.go          # 后端单元测试
├── frontend/
│   ├── tests/
│   │   ├── e2e/                   # E2E 测试
│   │   │   └── *.spec.ts
│   │   └── setup/                 # 测试启动器
│   │       ├── chatclaw-launcher.ts
│   │       └── chatclaw-teardown.ts
│   └── playwright.webview2.config.ts  # Playwright WebView2 配置
```

### 调试测试

```bash
# 带 UI 运行 Playwright
cd frontend
pnpm playwright test --config playwright.webview2.config.ts --headed

# 调试模式（暂停在第一行）
pnpm playwright test --config playwright.webview2.config.ts --debug
```

### 测试结果

测试结果保存在 `frontend/test-results/` 目录下，失败时会自动截图。

```bash
# 查看测试报告
cd frontend
npx playwright show-report
```
