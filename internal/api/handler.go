package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"windows-compile-service/internal/compiler"
)

// Handler 是编译服务的 HTTP 层入口。
// 它持有全局共享的并发信号量，用于执行服务要求的最大并发限制。
type Handler struct {
	compileSlots chan struct{}
}

// CompileRequest 对应 POST /compile 的 JSON 请求结构。
// 这里只接受源码字符串和少量高层编译开关，不接受任何原始命令行参数。
type CompileRequest struct {
	SourceCode      string                `json:"source_code" binding:"required"`
	CompilerOptions CompileRequestOptions `json:"compiler_options" binding:"required"`
}

// CompileRequestOptions 是暴露给调用方的编译选项。
// 这些字段会在 compiler 包内部映射成严格白名单中的 MSVC 参数，
// HTTP 层不会把用户输入直接拼接成 cl.exe 参数。
type CompileRequestOptions struct {
	OptLevel         string `json:"opt_level" binding:"required"`
	NoInline         bool   `json:"no_inline"`
	KeepFramePointer bool   `json:"keep_frame_pointer"`
}

// ErrorResponse 用于返回结构化错误，便于调用方稳定解析。
type ErrorResponse struct {
	Error string `json:"error"`
}

// NewHandler 把 main 中创建的全局并发信号量注入到 Handler。
// 这里保持实现简单，避免引入不必要的状态管理复杂度。
func NewHandler(compileSlots chan struct{}) *Handler {
	return &Handler{compileSlots: compileSlots}
}

// Compile 负责处理单次编译请求：
// 1. 校验请求体
// 2. 执行并发限制
// 3. 创建 30 秒超时上下文
// 4. 调用 compiler 层完成编译
// 5. 返回 multipart 产物（exe + build_log + meta）
//
// 即使 cl.exe 返回非 0，只要编译流程已经进入执行阶段，也仍然返回 build_log + meta，
// 编译成功时会额外包含 sample.exe 文件部分。
func (h *Handler) Compile(c *gin.Context) {
	var req CompileRequest
	// Gin 会根据结构体 tag 自动做必填字段校验。
	// 请求格式错误或字段缺失时，直接在进入编译层之前返回 400。
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
		return
	}

	// 非阻塞获取并发槽位：
	// 当 3 个编译槽位都被占满时，立刻返回 429，
	// 而不是无限排队，避免请求堆积导致整体延迟失控。
	select {
	case h.compileSlots <- struct{}{}:
		// 无论后续成功、失败还是超时，都必须释放槽位。
		defer func() { <-h.compileSlots }()
	default:
		c.JSON(http.StatusTooManyRequests, ErrorResponse{Error: "maximum concurrent compilations reached"})
		return
	}

	// HTTP 层在这里施加固定 30 秒的硬超时。
	// compiler 层内部通过 exec.CommandContext 绑定这个 ctx，
	// 一旦超时会终止 cmd.exe 以及后续的编译链路。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// 把传输层的请求结构转换成 compiler 包内部使用的领域模型，
	// 这样可以把 HTTP 协议细节和编译实现解耦。
	artifact, err := compiler.Compile(ctx, compiler.Job{
		SourceCode: req.SourceCode,
		Options: compiler.Options{
			OptLevel:         req.CompilerOptions.OptLevel,
			NoInline:         req.CompilerOptions.NoInline,
			KeepFramePointer: req.CompilerOptions.KeepFramePointer,
		},
	})
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, compiler.ErrInvalidCompilerOption):
			status = http.StatusBadRequest
		}

		// 内部错误写入服务端日志，供运维排查；
		// 客户端只收到一个简短且可机器解析的错误响应。
		log.Printf("compile failed: %v", err)
		c.JSON(status, ErrorResponse{Error: err.Error()})
		return
	}

	// 成功进入编译流程后，响应体固定为 multipart/form-data：
	// - part "artifact": sample.exe（二进制，可选）
	// - part "build_log": 编译日志（文本）
	// - part "meta": 元数据（JSON）
	body, contentType, err := buildMultipartResponse(artifact)
	if err != nil {
		log.Printf("build multipart response failed: %v", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "build response failed"})
		return
	}

	c.Header("Content-Type", contentType)
	c.Header("X-Compile-Return-Code", intToHeader(artifact.Meta.ReturnCode))
	c.Header("X-Compile-Timed-Out", strconv.FormatBool(artifact.Meta.TimedOut))
	c.Data(http.StatusOK, contentType, body)
}

// intToHeader 把整数转换成可写入 HTTP Header 的文本。
func intToHeader(value int) string {
	return strconv.Itoa(value)
}

func buildMultipartResponse(artifact compiler.Artifact) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if len(artifact.ExecutableData) > 0 {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="artifact"; filename="sample.exe"`)
		header.Set("Content-Type", "application/vnd.microsoft.portable-executable")
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(artifact.ExecutableData); err != nil {
			return nil, "", err
		}
	}

	buildLogPart, err := writer.CreateFormField("build_log")
	if err != nil {
		return nil, "", err
	}
	if _, err := buildLogPart.Write([]byte(artifact.BuildLog)); err != nil {
		return nil, "", err
	}

	metaJSON, err := json.Marshal(artifact.Meta)
	if err != nil {
		return nil, "", err
	}
	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Disposition", `form-data; name="meta"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(metaJSON); err != nil {
		return nil, "", err
	}

	if err := writer.Close(); err != nil {
		return nil, "", err
	}

	return buf.Bytes(), writer.FormDataContentType(), nil
}
