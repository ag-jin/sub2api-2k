//go:build unit

package handler

// 09 — 503 续跑计费/编排契约门禁。
//
// 本文件是源码级（AST + 字符串）契约测试，不启动 handler。它钉死 design
// 决策 5“handler 层零改动、编排在 service”：
//  1. Responses handler 内只有一处 service.OpenAIRecordUsageInput 构造点，
//     续跑必须复用同一个 submitResponsesUsage，不得新增第二条记账路径；
//  2. handler 源码不出现任何续跑编排标识（状态/信号/body 重写器/开关键）。
//
// 注意：openai_gateway_handler.go 全文件另有 Messages 与 WebSocket turn 两条
// 独立的记账路径（本功能之前已存在，见卡片 09 Comments 的基线说明），因此
// “唯一记账点”按 Responses handler 作用域断言，而不是按整个文件。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const openAIResponsesHandlerSourceFile = "openai_gateway_handler.go"

func TestOpenAIResponsesHandlerHasSingleUsageRecordSite(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", openAIResponsesHandlerSourceFile), nil, 0)
	require.NoError(t, err)

	responsesFn := findResponsesHandlerFunc(file)
	require.NotNil(t, responsesFn,
		"(*OpenAIGatewayHandler).Responses must exist: it owns the /v1/responses forward + billing path")

	var sites []token.Position
	ast.Inspect(responsesFn, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || !isOpenAIRecordUsageInputLiteral(literal.Type) {
			return true
		}
		sites = append(sites, fset.Position(literal.Lbrace))
		return true
	})

	require.Len(t, sites, 1,
		"Responses handler must keep exactly one service.OpenAIRecordUsageInput site; "+
			"503 continuation has to reuse submitResponsesUsage, not add a second billing path (found %v)", sites)
}

func TestOpenAIResponsesHandlerDoesNotOrchestrateContinuation(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", openAIResponsesHandlerSourceFile))
	require.NoError(t, err)
	code := string(src)

	for _, identifier := range []string{
		"openAIStreamContinuationState",
		"openAIStreamContinueSignal",
		"newOpenAIStreamContinuationBody",
		"openai_stream_503_auto_continue",
	} {
		require.NotContains(t, code, identifier,
			"continuation orchestration must stay in service; handler leaked %q", identifier)
	}
}

// findResponsesHandlerFunc 返回 (*OpenAIGatewayHandler).Responses 的声明。
func findResponsesHandlerFunc(file *ast.File) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name == nil || fn.Name.Name != "Responses" {
			continue
		}
		if len(fn.Recv.List) == 1 && isOpenAIGatewayHandlerPtr(fn.Recv.List[0].Type) {
			return fn
		}
	}
	return nil
}

// isOpenAIGatewayHandlerPtr 匹配 *OpenAIGatewayHandler 接收者类型。
func isOpenAIGatewayHandlerPtr(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "OpenAIGatewayHandler"
}
