package compiler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	// ErrInvalidCompilerOption 表示请求中的编译选项不符合服务端白名单。
	ErrInvalidCompilerOption = errors.New("invalid compiler option")
)

const (
	// baseTempRoot 是每次编译工作目录的固定根路径。
	// 每个请求都会在这个目录下创建一个以 UUID 命名的独立子目录。
	baseTempRoot = `C:\build\tmp`
)

// Job 是 compiler 层对一次编译任务的内部表示。
// 它包含源码内容，以及已经规整好的高层编译选项。
type Job struct {
	SourceCode string
	Options    Options
}

// Options 是用户唯一可以影响编译行为的输入集合。
// 这些字段最终会被映射成固定的、受控的 MSVC 参数。
type Options struct {
	OptLevel         string
	NoInline         bool
	KeepFramePointer bool
}

// Meta 会写入 meta.json，同时也会返回给 HTTP 层，
// 供响应头输出部分状态信息。
type Meta struct {
	CompiledAt      string   `json:"compiled_at"`
	CompilerOptions []string `json:"compiler_options"`
	ReturnCode      int      `json:"return_code"`
	TimedOut        bool     `json:"timed_out"`
	WorkDir         string   `json:"work_dir"`
}

// Artifact 是单次编译完成后在内存中的结果对象。
// ExecutableData 在编译成功且 sample.exe 存在时才有值。
type Artifact struct {
	ExecutableData []byte
	BuildLog       string
	Meta           Meta
}

// Compile 完成一次完整编译流程：
// 1. 校验源码
// 2. 将高层选项映射为白名单参数
// 3. 定位 vcvars64.bat
// 4. 创建独立临时目录
// 5. 写入 sample.c
// 6. 执行 cmd.exe -> vcvars64.bat -> cl.exe
// 7. 收集 build.log
// 8. 生成 meta.json
// 9. 读取产物到内存
// 10. 保留临时目录供后续排查
//
// 这里不会执行任何用户提供的 shell 片段；请求中的选项只能转成固定白名单参数。
func Compile(ctx context.Context, job Job) (Artifact, error) {
	// 提前拒绝空源码，避免无意义地创建目录或探测 VS 安装。
	if strings.TrimSpace(job.SourceCode) == "" {
		return Artifact{}, fmt.Errorf("source_code is required")
	}

	// 把外部请求参数转换成唯一允许的编译参数集合。
	// 这是参数白名单校验的核心入口。
	flags, err := resolveCompilerFlags(job.Options)
	if err != nil {
		return Artifact{}, err
	}

	// 动态发现 vcvars64.bat 路径，兼容标准的 Visual Studio Build Tools 安装。
	vcvarsPath, err := setupMSVCEnv()
	if err != nil {
		return Artifact{}, err
	}

	// 每个请求都在 C:\build\tmp\<uuid> 下独立运行。
	// 目录会保留，便于后续查看源码、脚本、日志、meta 和产物文件。
	workDir, err := createWorkDir()
	if err != nil {
		return Artifact{}, fmt.Errorf("create work directory: %w", err)
	}

	// 按约定文件名写入源码，供 cl.exe 编译使用。
	sourcePath := filepath.Join(workDir, "sample.c")
	if err := os.WriteFile(sourcePath, []byte(job.SourceCode), 0o644); err != nil {
		return Artifact{}, fmt.Errorf("write source file: %w", err)
	}

	// build.log 记录完整的编译输出。
	// 即使编译失败，也会通过响应返回，方便调用方查看错误。
	logPath := filepath.Join(workDir, "build.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return Artifact{}, fmt.Errorf("create build log: %w", err)
	}
	defer logFile.Close()

	// 为了避免 cmd.exe 在单行命令模式下的复杂引号解析问题，
	// 这里改为先生成一个临时 build.cmd，再执行这个脚本。
	// 这样包含空格的 vcvars64.bat 路径会稳定很多。
	scriptPath := filepath.Join(workDir, "build.cmd")
	if err := writeBuildScript(scriptPath, vcvarsPath, flags); err != nil {
		return Artifact{}, err
	}

	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", "build.cmd")
	// 强制在独立工作目录中执行，保证 sample.c、sample.exe、build.log、
	// meta.json 都生成在当前请求自己的目录里。
	cmd.Dir = workDir

	// stdout 和 stderr 都写入同一个日志文件，完整保留 cmd.exe 和 cl.exe 的输出。
	multiWriter := io.MultiWriter(logFile)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter

	start := time.Now()
	// CommandContext 会监听 ctx。
	// 当上层 30 秒超时到达时，会终止当前命令链，落实硬超时要求。
	runErr := cmd.Run()
	duration := time.Since(start)

	// 返回码默认为 0，表示编译成功。
	// 非 0 返回码会写入元数据，用来区分“编译失败”和“服务调用失败”。
	returnCode := 0
	timedOut := false

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// 用 -1 明确表示这是服务超时主动终止，
			// 而不是 cl.exe 自己返回的退出码。
			timedOut = true
			returnCode = -1
		} else {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				returnCode = exitErr.ExitCode()
			} else {
				return Artifact{}, fmt.Errorf("run compiler: %w", runErr)
			}
		}
	}

	// 在日志末尾附加耗时信息，便于快速查看，不必额外打开 meta.json。
	if _, err := io.WriteString(logFile, "\n---\ncompile duration: "+duration.String()+"\n"); err != nil {
		return Artifact{}, fmt.Errorf("append build log: %w", err)
	}

	// 无论编译成功、失败还是超时，都会生成 meta.json，
	// 这样下游始终能拿到一致的元数据结构。
	meta := Meta{
		CompiledAt:      start.UTC().Format(time.RFC3339),
		CompilerOptions: flags,
		ReturnCode:      returnCode,
		TimedOut:        timedOut,
		WorkDir:         workDir,
	}

	metaPath := filepath.Join(workDir, "meta.json")
	if err := writeMeta(metaPath, meta); err != nil {
		return Artifact{}, err
	}

	buildLogData, err := os.ReadFile(logPath)
	if err != nil {
		return Artifact{}, fmt.Errorf("read build log: %w", err)
	}

	// sample.exe 只有在编译成功时才存在。
	executablePath := filepath.Join(workDir, "sample.exe")
	var executableData []byte
	if _, err := os.Stat(executablePath); err == nil {
		executableData, err = os.ReadFile(executablePath)
		if err != nil {
			return Artifact{}, fmt.Errorf("read executable: %w", err)
		}
	}

	return Artifact{
		ExecutableData: executableData,
		BuildLog:       string(buildLogData),
		Meta:           meta,
	}, nil
}

// setupMSVCEnv 先定位 vswhere.exe，再用它查询最新的 Visual Studio 安装目录，
// 最终返回 vcvars64.bat 的完整路径。
// 函数签名按要求固定，方便其他包直接依赖这个入口。
func setupMSVCEnv() (string, error) {
	// 该服务只面向 Windows。
	// 在非 Windows 环境下尽早失败，可以让开发阶段的问题更明显。
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("setupMSVCEnv requires Windows")
	}

	vswherePath, err := findVSWhere()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(
		vswherePath,
		"-latest",
		"-products", "*",
		"-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
		"-property", "installationPath",
	)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run vswhere.exe: %w", err)
	}

	// 使用 -latest 时，vswhere 预期返回一个安装目录。
	installPath := strings.TrimSpace(string(output))
	if installPath == "" {
		return "", fmt.Errorf("vswhere.exe did not return a Visual Studio installation path")
	}

	// vcvars64.bat 是用于初始化 x64 编译工具链环境变量的标准脚本，
	// 会设置 PATH、INCLUDE、LIB 等关键变量。
	vcvarsPath := filepath.Join(installPath, "VC", "Auxiliary", "Build", "vcvars64.bat")
	if _, err := os.Stat(vcvarsPath); err != nil {
		return "", fmt.Errorf("vcvars64.bat not found at %s: %w", vcvarsPath, err)
	}

	return vcvarsPath, nil
}

// findVSWhere 在标准的 Visual Studio Installer 目录中查找 vswhere.exe。
// 这里只检查固定路径，避免全盘扫描带来的不确定性和额外开销。
func findVSWhere() (string, error) {
	roots := []string{
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramFiles"),
	}

	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}

		candidate := filepath.Join(root, "Microsoft Visual Studio", "Installer", "vswhere.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("vswhere.exe not found in Visual Studio Installer directory")
}

// createWorkDir 确保根目录存在，然后创建一个当前请求独占的子目录。
// 调用方负责在使用后删除它。
func createWorkDir() (string, error) {
	if err := os.MkdirAll(baseTempRoot, 0o755); err != nil {
		return "", err
	}

	dirName, err := newRequestID()
	if err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}

	workDir := filepath.Join(baseTempRoot, dirName)
	if err := os.Mkdir(workDir, 0o755); err != nil {
		return "", err
	}

	return workDir, nil
}

// resolveCompilerFlags 把用户可见的高层选项映射成服务允许的精确 MSVC 参数。
// 这里不接受任何任意字符串参数，从而避免参数注入。
func resolveCompilerFlags(options Options) ([]string, error) {
	var flags []string

	switch options.OptLevel {
	case "Od":
		flags = append(flags, "/Od")
	case "O2":
		flags = append(flags, "/O2")
	default:
		return nil, fmt.Errorf("%w: opt_level must be Od or O2", ErrInvalidCompilerOption)
	}

	if options.NoInline {
		flags = append(flags, "/Ob0")
	}

	if options.KeepFramePointer {
		flags = append(flags, "/Oy-")
	}

	// /GS- 由服务端固定追加，不允许客户端直接控制，
	// 这样可以避免用户借机注入其他原始参数。
	flags = append(flags, "/GS-")
	return flags, nil
}

// writeMeta 把编译元数据序列化为稳定、可读的 JSON，
// 供打包进 ZIP 返回给调用方。
func writeMeta(path string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta.json: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	return nil
}

// writeBuildScript 生成当前请求专用的批处理脚本。
// 脚本先调用 vcvars64.bat 初始化环境，再调用 cl.exe 编译 sample.c。
func writeBuildScript(path, vcvarsPath string, flags []string) error {
	lines := []string{
		"@echo off",
		`call "` + vcvarsPath + `"`,
		"if errorlevel 1 exit /b %errorlevel%",
		buildCompileCommand(flags),
	}

	content := strings.Join(lines, "\r\n") + "\r\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write build script: %w", err)
	}

	return nil
}

// buildCompileCommand 只负责生成 cl.exe 命令本体。
// 这里不包含任何用户自定义字符串，只拼接服务端白名单参数。
func buildCompileCommand(flags []string) string {
	parts := []string{
		"cl.exe",
		"/nologo",
		"/TC",
		"/Fe:sample.exe",
		"sample.c",
	}

	parts = append(parts, flags...)
	return strings.Join(parts, " ")
}

// newRequestID 生成一个 UUIDv4 字符串，不引入第三方依赖。
// 该值直接用作每次请求的工作目录名。
func newRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}

	// 按 RFC 4122 设置版本号和变体位，确保生成的是合法 UUID。
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		raw[0:4],
		raw[4:6],
		raw[6:8],
		raw[8:10],
		raw[10:16],
	), nil
}
