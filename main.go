package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/TobiasYin/go-lsp/logs"
	"github.com/TobiasYin/go-lsp/lsp"
	"github.com/TobiasYin/go-lsp/lsp/defines"
	tree_sitter_hdl "github.com/quantonganh/tree-sitter-hdl/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

func main() {
	logger := log.New(os.Stdout, "hdls: ", log.LstdFlags)
	logs.Init(logger)

	parser := tree_sitter.NewParser()
	defer parser.Close()

	if err := parser.SetLanguage(tree_sitter.NewLanguage(tree_sitter_hdl.Language())); err != nil {
		log.Fatal(err)
	}

	server := lsp.NewServer(&lsp.Options{})

	server.OnInitialize(func(ctx context.Context, req *defines.InitializeParams) (result *defines.InitializeResult, err *defines.InitializeError) {
		ir := &defines.InitializeResult{}
		ir.Capabilities.TextDocumentSync = defines.TextDocumentSyncKindFull
		ir.Capabilities.DefinitionProvider = true
		return ir, nil
	})

	server.OnInitialized(func(ctx context.Context, req *defines.InitializeParams) error {
		return nil
	})

	primitiveChips := make(map[string]struct{})
	openFiles := make(map[string][]byte)

	publishDiagnostics := func(source []byte, uri defines.DocumentUri, version int) error {
		tree := parser.Parse(source, nil)
		defer tree.Close()

		var compositeChipName string
		var walk func(n *tree_sitter.Node)
		diagnostics := make([]defines.Diagnostic, 0)
		walk = func(n *tree_sitter.Node) {
			switch strings.TrimSpace(n.Kind()) {
			case "chip_definition":
				if name := n.ChildByFieldName("name"); name != nil {
					compositeChipName = string(source[name.StartByte():name.EndByte()])
				}
			case "part":
				if name := n.ChildByFieldName("chip_name"); name != nil {
					primitive := string(source[name.StartByte():name.EndByte()])

					if primitive == compositeChipName {
						diagnostics = append(diagnostics, newDiagnostic(name, fmt.Sprintf("Cannot use chip %s to implement itself", primitive)))
					}

					if _, ok := primitiveChips[primitive]; !ok {
						diagnostics = append(diagnostics, newDiagnostic(name, fmt.Sprintf("Undefined chip name: %s", primitive)))
					}
				}
			}

			for i := 0; i < int(n.NamedChildCount()); i++ {
				walk(n.NamedChild(uint(i)))
			}
		}

		walk(tree.RootNode())

		diagnosticsParams := defines.PublishDiagnosticsParams{
			Uri:         uri,
			Version:     &version,
			Diagnostics: diagnostics,
		}
		params, err := json.Marshal(diagnosticsParams)
		if err != nil {
			return fmt.Errorf("error marshalling diagnostic params: %w", err)
		}

		if err := server.SendNotification("textDocument/publishDiagnostics", json.RawMessage(params)); err != nil {
			return fmt.Errorf("error sending notification: %w", err)
		}

		return nil
	}

	server.OnDidOpenTextDocument(func(ctx context.Context, req *defines.DidOpenTextDocumentParams) error {
		uri := string(req.TextDocument.Uri)
		source := []byte(req.TextDocument.Text)
		openFiles[uri] = source

		if err := collectPrimitiveChips(builtInChipsDir(uri), primitiveChips); err != nil {
			return fmt.Errorf("collect primitive builtin chips: %w", err)
		}

		if err := collectPrimitiveChips(filepath.Dir(toFilePath(uri)), primitiveChips); err != nil {
			return fmt.Errorf("collect implemented chips: %w", err)
		}

		if err := publishDiagnostics(source, req.TextDocument.Uri, req.TextDocument.Version); err != nil {
			return err
		}

		return nil
	})

	server.OnDidChangeTextDocument(func(ctx context.Context, req *defines.DidChangeTextDocumentParams) error {
		for _, contentChange := range req.ContentChanges {
			if err := publishDiagnostics([]byte(contentChange.Text.(string)), req.TextDocument.Uri, req.TextDocument.Version); err != nil {
				return err
			}
		}

		return nil
	})

	server.OnDefinition(func(ctx context.Context, req *defines.DefinitionParams) (result *[]defines.LocationLink, err error) {
		source, err := readFile(string(req.TextDocument.Uri))
		if err != nil {
			return nil, err
		}

		offset := getByteOffset(string(source), int(req.Position.Line), int(req.Position.Character))
		log.Printf("offset: %d", offset)

		tree := parser.Parse(source, nil)
		defer tree.Close()

		node := tree.RootNode().DescendantForByteRange(uint(offset), uint(offset)).Parent()
		log.Printf("kind: %s", node.Kind())
		if strings.TrimSuffix(node.Kind(), "\n") == "part" {
			if name := node.ChildByFieldName("chip_name"); name != nil {
				primitiveChipName := string(source[name.StartByte():name.EndByte()])
				targetPath := filepath.Join(builtInChipsDir(string(req.TextDocument.Uri)), primitiveChipName+".hdl")
				result := &[]defines.LocationLink{
					{
						TargetUri: defines.DocumentUri("file://" + targetPath),
					},
				}
				return result, nil
			}
		}

		return nil, nil
	})

	server.Run()
}

func readFile(uri string) ([]byte, error) {
	source, err := os.ReadFile(toFilePath(uri))
	if err != nil {
		return nil, err
	}
	return source, nil
}

func collectPrimitiveChips(dir string, chips map[string]struct{}) error {
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && filepath.Ext(path) == ".hdl" {
			chip := strings.TrimSuffix(filepath.Base(path), ".hdl")
			chips[chip] = struct{}{}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("error walking dir: %w", err)
	}

	return nil
}

func toFilePath(uri string) string {
	enEscapeUrl, _ := url.QueryUnescape(strings.TrimSpace(uri))
	return strings.TrimPrefix(enEscapeUrl, "file:")
}

func getByteOffset(text string, line, char int) int {
	lines := strings.Split(text, "\n")
	if line > len(lines) {
		return len(text)
	}

	offset := 0
	for i := 0; i < line; i++ {
		offset += len(lines[i]) + 1
	}

	offset += len([]byte(lines[line][:char]))
	return offset
}

func builtInChipsDir(uri string) string {
	baseDir := filepath.Join(filepath.Dir(toFilePath(uri)), "..", "..")
	baseDir = filepath.Clean(baseDir)
	return filepath.Join(baseDir, "tools", "builtInChips")
}

func newDiagnostic(n *tree_sitter.Node, msg string) defines.Diagnostic {
	severity := defines.DiagnosticSeverityError
	return defines.Diagnostic{
		Range: defines.Range{
			Start: defines.Position{
				Line:      n.StartPosition().Row,
				Character: n.StartPosition().Column,
			},
			End: defines.Position{
				Line:      n.EndPosition().Row,
				Character: n.EndPosition().Column,
			},
		},
		Severity: &severity,
		Message:  msg,
	}
}
