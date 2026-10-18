package configuration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"text/template"
	"text/template/parse"
)

const MaxTemplateBytes = 64 * 1024
const MaxTemplateSteps = 100000
const maxTemplateDataItems = 8192

var (
	ErrTemplateInvalid = errors.New("invalid template")
	ErrTemplateLimit   = errors.New("template limit exceeded")
	ErrDigestMismatch  = errors.New("rendered digest mismatch")
)

// TemplateSpec is deliberately JSON-only. The caller supplies the complete
// inventory; Fabric neither filters peers nor injects host process state.
type TemplateSpec struct {
	Engine string         `json:"engine"`
	Source string         `json:"source"`
	Data   map[string]any `json:"data"`
}

type boundedOutput struct {
	ctx context.Context
	bytes.Buffer
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.Len()+len(p) > MaxFileBytes {
		return 0, ErrTemplateLimit
	}
	return w.Buffer.Write(p)
}

func validateData(value any, depth int, items *int, size *int) error {
	*items++
	if depth > 32 || *items > maxTemplateDataItems {
		return ErrTemplateLimit
	}
	switch v := value.(type) {
	case nil, bool, float64, int, int64:
	case string:
		*size += len(v)
	case []any:
		for _, x := range v {
			if err := validateData(x, depth+1, items, size); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, x := range v {
			*size += len(k)
			if err := validateData(x, depth+1, items, size); err != nil {
				return err
			}
		}
	default:
		return ErrTemplateInvalid
	}
	if *size > MaxFileBytes {
		return ErrTemplateLimit
	}
	return nil
}

func guardTree(node parse.Node, depth int, count *int, step parse.Node) error {
	if node == nil {
		return nil
	}
	*count++
	if depth > 64 || *count > 4096 {
		return ErrTemplateLimit
	}
	visit := func(n parse.Node) error { return guardTree(n, depth+1, count, step) }
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		original := n.Nodes
		guarded := []parse.Node{step.Copy()}
		for _, child := range original {
			if err := visit(child); err != nil {
				return err
			}
			guarded = append(guarded, step.Copy(), child)
		}
		n.Nodes = guarded
	case *parse.ActionNode:
		return visit(n.Pipe)
	case *parse.PipeNode:
		for _, c := range n.Cmds {
			if err := visit(c); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			if err := visit(a); err != nil {
				return err
			}
		}
	case *parse.IdentifierNode:
		switch n.Ident {
		case "and", "or", "not", "eq", "ne", "lt", "le", "gt", "ge", "len":
		default:
			return ErrTemplateInvalid
		}
	case *parse.IfNode:
		if err := visit(n.Pipe); err != nil {
			return err
		}
		if err := visit(n.List); err != nil {
			return err
		}
		return visit(n.ElseList)
	case *parse.RangeNode:
		if err := visit(n.Pipe); err != nil {
			return err
		}
		if err := visit(n.List); err != nil {
			return err
		}
		return visit(n.ElseList)
	case *parse.WithNode:
		if err := visit(n.Pipe); err != nil {
			return err
		}
		if err := visit(n.List); err != nil {
			return err
		}
		return visit(n.ElseList)
	case *parse.ChainNode:
		return visit(n.Node)
	case *parse.TemplateNode:
		return ErrTemplateInvalid
	case *parse.TextNode, *parse.FieldNode, *parse.VariableNode, *parse.DotNode,
		*parse.NilNode, *parse.BoolNode, *parse.NumberNode, *parse.StringNode,
		*parse.BreakNode, *parse.ContinueNode, *parse.CommentNode:
	default:
		return ErrTemplateInvalid
	}
	return nil
}

// RenderTemplate executes a bounded Go text/template dialect. Every evaluated
// block checks the execution budget, including loops that produce no output.
func RenderTemplate(ctx context.Context, spec TemplateSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if spec.Engine != "go-template/v1" {
		return "", ErrTemplateInvalid
	}
	if len(spec.Source) > MaxTemplateBytes {
		return "", ErrTemplateLimit
	}
	var items, size int
	if err := validateData(spec.Data, 0, &items, &size); err != nil {
		return "", err
	}
	tmpl, err := template.New("file").Option("missingkey=error").Parse(spec.Source)
	if err != nil || len(tmpl.Templates()) != 1 {
		return "", ErrTemplateInvalid
	}
	steps := 0
	tick := func() (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		steps++
		if steps > MaxTemplateSteps {
			return "", ErrTemplateLimit
		}
		return "", nil
	}
	helper := template.Must(template.New("guard").Funcs(template.FuncMap{"__fabric_step": tick}).Parse("{{__fabric_step}}"))
	count := 0
	if err := guardTree(tmpl.Tree.Root, 0, &count, helper.Tree.Root.Nodes[0]); err != nil {
		return "", err
	}
	tmpl.Funcs(template.FuncMap{"__fabric_step": tick})
	output := &boundedOutput{ctx: ctx}
	if err := tmpl.Execute(io.Writer(output), spec.Data); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, ErrTemplateLimit) {
			return "", ErrTemplateLimit
		}
		return "", ErrTemplateInvalid
	}
	return output.String(), nil
}
