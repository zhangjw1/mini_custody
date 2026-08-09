package style

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestFunctionsHaveChineseComments 确保所有函数和测试辅助函数都保留中文说明。
func TestFunctionsHaveChineseComments(t *testing.T) {
	root := moduleRoot(t)
	walkGoFiles(t, root, func(path string, file *ast.File, fset *token.FileSet) {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if function.Doc == nil || !containsChinese(function.Doc.Text()) {
				position := fset.Position(function.Pos())
				t.Errorf("%s:%d 函数 %s 缺少中文注释", relativePath(root, path), position.Line, function.Name.Name)
			}
		}
	})
}

// TestRuntimeMessagesUseChinese 确保运行时固定消息使用中文。
func TestRuntimeMessagesUseChinese(t *testing.T) {
	root := moduleRoot(t)
	walkGoFiles(t, root, func(path string, file *ast.File, fset *token.FileSet) {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			argument, ok := runtimeMessageArgument(call)
			if !ok {
				return true
			}
			literal, ok := argument.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			message, err := strconv.Unquote(literal.Value)
			if err == nil && !containsChinese(message) {
				position := fset.Position(literal.Pos())
				t.Errorf("%s:%d 运行时消息必须包含中文：%q", relativePath(root, path), position.Line, message)
			}
			return true
		})
	})
}

// moduleRoot 返回当前 Go Module 的根目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位规范测试文件")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

// walkGoFiles 遍历并解析全部 Go 源文件。
func walkGoFiles(t *testing.T, root string, visit func(string, *ast.File, *token.FileSet)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		visit(path, file, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 Go 代码失败：%v", err)
	}
}

// runtimeMessageArgument 返回调用中承载运行时消息的参数。
func runtimeMessageArgument(call *ast.CallExpr) (ast.Expr, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	receiver, isIdent := selector.X.(*ast.Ident)
	if isIdent && receiver.Name == "errors" && selector.Sel.Name == "New" && len(call.Args) > 0 {
		return call.Args[0], true
	}
	if isIdent && receiver.Name == "fmt" {
		switch selector.Sel.Name {
		case "Errorf", "Print", "Printf", "Println":
			if len(call.Args) > 0 {
				return call.Args[0], true
			}
		case "Fprint", "Fprintf", "Fprintln":
			if len(call.Args) > 1 {
				return call.Args[1], true
			}
		}
	}
	if selector.Sel.Name == "Error" || selector.Sel.Name == "Warn" ||
		selector.Sel.Name == "Info" || selector.Sel.Name == "Debug" {
		if isLoggerReceiver(selector.X) && len(call.Args) > 0 {
			return call.Args[0], true
		}
	}
	return nil, false
}

// isLoggerReceiver 判断调用接收者是否为结构化日志记录器。
func isLoggerReceiver(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name == "logger" || value.Name == "slog"
	case *ast.SelectorExpr:
		return value.Sel.Name == "logger"
	default:
		return false
	}
}

// containsChinese 判断文本是否至少包含一个汉字。
func containsChinese(value string) bool {
	return strings.IndexFunc(value, func(value rune) bool {
		return unicode.Is(unicode.Han, value)
	}) >= 0
}

// relativePath 返回便于测试输出定位的模块内相对路径。
func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return relative
}
