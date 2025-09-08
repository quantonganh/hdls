package main

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	ext = ".hdl"

	nodeKindChipDefinition = "chip_definition"
	nodeKindPart           = "part"
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

	implementedChips := make(map[string]struct{})
	openFiles := make(map[string][]byte)

	publishDiagnostics := func(source []byte, uri defines.DocumentUri, version int) error {
		tree := parser.Parse(source, nil)
		defer tree.Close()

		var implementingChipName string
		var walk func(n *tree_sitter.Node)
		diagnostics := make([]defines.Diagnostic, 0)
		walk = func(n *tree_sitter.Node) {
			switch strings.TrimSpace(n.Kind()) {
			case nodeKindChipDefinition:
				if name := n.ChildByFieldName("name"); name != nil {
					implementingChipName = string(source[name.StartByte():name.EndByte()])
				}
			case nodeKindPart:
				if name := n.ChildByFieldName("chip_name"); name != nil {
					chipName := string(source[name.StartByte():name.EndByte()])

					if chipName == implementingChipName {
						diagnostics = append(diagnostics, newDiagnostic(name, fmt.Sprintf("Cannot use chip %s to implement itself", chipName)))
					}

					if _, ok := implementedChips[chipName]; !ok {
						diagnostics = append(diagnostics, newDiagnostic(name, fmt.Sprintf("Undefined chip name: %s", chipName)))
					}
				}

				for i := 0; i < int(n.ChildCount()); i++ {
					child := n.Child(uint(i))
					if child.IsError() {
						childText := string(source[child.StartByte():child.EndByte()])
						if strings.HasPrefix(strings.TrimSpace(childText), ")") {
							diagnostics = append(diagnostics, newDiagnostic(child.Child(1), "Expected \";\""))
						}
						break
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

		if err := collectChips(builtInChipsDir(uri), implementedChips); err != nil {
			return fmt.Errorf("collect primitive builtin chips: %w", err)
		}

		if err := collectChips(baseDir(uri), implementedChips); err != nil {
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
		uri := string(req.TextDocument.Uri)
		source, err := readFile(uri)
		if err != nil {
			return nil, err
		}

		offset := getByteOffset(string(source), int(req.Position.Line), int(req.Position.Character))

		tree := parser.Parse(source, nil)
		defer tree.Close()

		node := tree.RootNode().DescendantForByteRange(uint(offset), uint(offset)).Parent()
		if strings.TrimSuffix(node.Kind(), "\n") == nodeKindPart {
			if name := node.ChildByFieldName("chip_name"); name != nil {
				primitiveChipName := string(source[name.StartByte():name.EndByte()])
				fileName := primitiveChipName + ext
				var targetUri string
				path := filepath.Join(baseDir(uri), fileName)
				_, err := os.Stat(path)
				if errors.Is(err, os.ErrNotExist) {
					targetUri = filepath.Join(builtInChipsDir(uri), fileName)
				} else {
					targetUri = path
				}

				targetSource, err := readFile(targetUri)
				if err != nil {
					return nil, err
				}

				targetTree := parser.Parse(targetSource, nil)
				defer targetTree.Close()

				var (
					walk          func(n *tree_sitter.Node)
					startPosition tree_sitter.Point
					endPosition   tree_sitter.Point
				)
				walk = func(n *tree_sitter.Node) {
					switch strings.TrimSpace(n.Kind()) {
					case nodeKindChipDefinition:
						if name := n.ChildByFieldName("name"); name != nil {
							startPosition = name.StartPosition()
							endPosition = name.EndPosition()

						}
					}

					for i := 0; i < int(n.NamedChildCount()); i++ {
						walk(n.NamedChild(uint(i)))
					}
				}

				walk(tree.RootNode())

				targetRange := defines.Range{
					Start: defines.Position{
						Line:      startPosition.Row,
						Character: startPosition.Column,
					},
					End: defines.Position{
						Line:      endPosition.Row,
						Character: endPosition.Column,
					},
				}
				result := &[]defines.LocationLink{
					{
						TargetUri:            defines.DocumentUri("file://" + targetUri),
						TargetRange:          targetRange,
						TargetSelectionRange: targetRange,
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

func collectChips(dir string, chips map[string]struct{}) error {
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && filepath.Ext(path) == ext {
			chip := strings.TrimSuffix(filepath.Base(path), ext)
			chips[chip] = struct{}{}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("error walking dir: %w", err)
	}

	return nil
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
	baseDir := filepath.Join(baseDir(uri), "..", "..")
	baseDir = filepath.Clean(baseDir)
	return filepath.Join(baseDir, "tools", "builtInChips")
}

func baseDir(uri string) string {
	return filepath.Dir(toFilePath(uri))
}

func toFilePath(uri string) string {
	enEscapeUrl, _ := url.QueryUnescape(strings.TrimSpace(uri))
	return strings.TrimPrefix(enEscapeUrl, "file:")
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
