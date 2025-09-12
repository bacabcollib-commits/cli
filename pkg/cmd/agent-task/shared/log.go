package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/cli/cli/v2/pkg/markdown"
)

//go:generate moq -rm -out log_mock.go . LogRenderer

type LogRenderer interface {
	Follow(fetcher func() ([]byte, error), w io.Writer, io *iostreams.IOStreams) error
	Render(logs []byte, w io.Writer, io *iostreams.IOStreams) (stop bool, err error)
}

type logRenderer struct{}

func NewLogRenderer() LogRenderer {
	return &logRenderer{}
}

func (r *logRenderer) Follow(fetcher func() ([]byte, error), w io.Writer, io *iostreams.IOStreams) error {
	var last string
	for {
		raw, err := fetcher()
		if err != nil {
			return err
		}

		logs := string(raw)
		if logs == last {
			continue
		}

		diff := strings.TrimSpace(logs[len(last):])

		if stop, err := r.Render([]byte(diff), w, io); err != nil {
			return err
		} else if stop {
			return nil
		}

		last = logs
	}
}

func (r *logRenderer) Render(logs []byte, w io.Writer, io *iostreams.IOStreams) (bool, error) {
	lines := slices.DeleteFunc(strings.Split(string(logs), "\n"), func(line string) bool {
		return line == ""
	})

	for _, line := range lines {
		raw, found := strings.CutPrefix(line, "data: ")
		if !found {
			return false, errors.New("unexpected log format")
		}

		// The only log entry type we're interested in is a chat completion chunk,
		// which can be verified by a successful unmarshal into the corresponding
		// type AND the Object field being equal to "chat.completion.chunk". The
		// latter is to avoid accepting an empty JSON object (i.e. "{}"). Also,
		// if the entry is not what we expect, we should just skip and avoid
		// returning an error.
		var entry chatCompletionChunkEntry
		err := json.Unmarshal([]byte(raw), &entry)
		if err != nil || entry.Object != "chat.completion.chunk" {
			continue
		}

		if stop, err := renderLogEntry(entry, w, io); err != nil {
			return false, fmt.Errorf("failed to process log entry: %w", err)
		} else if stop {
			return true, nil
		}
	}

	return false, nil
}

func renderLogEntry(entry chatCompletionChunkEntry, w io.Writer, io *iostreams.IOStreams) (bool, error) {
	cs := io.ColorScheme()
	var stop bool
	for _, choice := range entry.Choices {
		if choice.FinishReason == "stop" {
			stop = true
		}

		if len(choice.Delta.ToolCalls) == 0 {
			if choice.Delta.Content != "" && choice.Delta.Role == "assistant" {
				// Copilot message and we should display.
				renderRawMarkdown(choice.Delta.Content, w, io)
			}
			continue
		}

		// Since we don't want to clear-and-reprint live progress of events, we
		// need to only process entries that correspond to a finished tool call.
		// Such entries have a non-empty Content field.
		if choice.Delta.Content == "" {
			continue
		}

		if choice.Delta.ReasoningText != "" {
			// Note that this should be formatted as a normal Copilot message.
			renderRawMarkdown(choice.Delta.ReasoningText, w, io)
		}

		for _, tc := range choice.Delta.ToolCalls {
			name := tc.Function.Name
			if name == "" {
				continue
			}

			args := tc.Function.Arguments

			switch name {
			case "run_setup":
				if v := unmarshal[runSetupToolArgs](args); v != nil {
					renderToolCall(w, cs, "Start "+v.Name+" MCP server", "")
					continue
				}
			case "view":
				args := viewToolArgs{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return false, fmt.Errorf("failed to parse 'view' tool call arguments: %w", err)
				}
				fmt.Fprintf(w, "View %s\n", cs.Bold(relativePath(args.Path)))

				// TODO: Strip the diff formatting from this, but for now render as it is.
				if err := renderFileContentAsMarkdown("output.diff", choice.Delta.Content, w, io); err != nil {
					return false, fmt.Errorf("failed to render viewed file content: %w", err)
				}
			case "bash":
				if v := unmarshal[bashToolArgs](args); v != nil {
					if v.Description != "" {
						renderToolCall(w, cs, "Bash", v.Description)
					} else {
						renderToolCall(w, cs, "Run Bash command", "")
					}

					contentWithCommand := choice.Delta.Content
					if v.Command != "" {
						contentWithCommand = fmt.Sprintf("%s\n%s", v.Command, choice.Delta.Content)
					}
					if err := renderFileContentAsMarkdown("commands.sh", contentWithCommand, w, io); err != nil {
						return false, fmt.Errorf("failed to render bash command output: %w", err)
					}
				}

			// GUI does not currently support these.
			// case "write_bash":
			// 	if v := unmarshal[writeBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("Send input to Bash session " + v.SessionID)
			// 		continue
			// 	}
			// case "read_bash":
			// 	if v := unmarshal[readBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("Read logs from Bash session " + v.SessionID)
			// 		continue
			// 	}
			// case "stop_bash":
			// 	if v := unmarshal[stopBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("Stop Bash session " + v.SessionID)
			// 		continue
			// 	}
			// case "async_bash":
			// 	if v := unmarshal[asyncBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("Start or send input to long-running Bash session " + v.SessionID)
			// 		continue
			// 	}
			// case "read_async_bash":
			// 	if v := unmarshal[readAsyncBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("View logs from long-running Bash session " + v.SessionID)
			// 		continue
			// 	}
			// case "stop_async_bash":
			// 	if v := unmarshal[stopAsyncBashToolArgs](args); v != nil {
			// 		renderToolCallTitle("Stop long-running Bash session " + v.SessionID)
			// 		continue
			// 	}
			case "think":
				args := thinkToolArgs{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return false, fmt.Errorf("failed to parse 'think' tool call arguments: %w", err)
				}

				// NOTE: omit the delta.content since it's the same as thought
				renderToolCall(w, cs, "Thought", "")
				if err := renderRawMarkdown(args.Thought, w, io); err != nil {
					return false, fmt.Errorf("failed to render thought: %w", err)
				}
			case "report_progress":
				args := reportProgressToolArgs{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return false, fmt.Errorf("failed to parse 'report_progress' tool call arguments: %w", err)
				}

				renderToolCall(w, cs, "Progress update", cs.Bold(args.CommitMessage))
				if args.PrDescription != "" {
					if err := renderRawMarkdown(args.PrDescription, w, io); err != nil {
						return false, fmt.Errorf("failed to render PR description: %w", err)
					}
				}

				// TODO: KW I wasn't able to get this to populate.
				if choice.Delta.Content != "" {
					// Try to treat this as JSON
					if err := renderContentAsJSONMarkdown(choice.Delta.Content, w, io); err != nil {
						return false, fmt.Errorf("failed to render progress update content: %w", err)
					}
				}

			case "create":
				args := createToolArgs{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return false, fmt.Errorf("failed to parse 'create' tool call arguments: %w", err)
				}
				renderToolCall(w, cs, "Create", cs.Bold(relativePath(args.Path)))

				if err := renderFileContentAsMarkdown(args.Path, args.FileText, w, io); err != nil {
					return false, fmt.Errorf("failed to render created file content: %w", err)
				}
			case "str_replace":
				args := strReplaceToolArgs{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return false, fmt.Errorf("failed to parse 'str_replace' tool call arguments: %w", err)
				}

				renderToolCall(w, cs, "Edit", cs.Bold(relativePath(args.Path)))
				if err := renderFileContentAsMarkdown("output.diff", choice.Delta.Content, w, io); err != nil {
					return false, fmt.Errorf("failed to render str_replace diff: %w", err)
				}
			default:
				// Unknown tool call. For example for "codeql_checker":
				// NOTE: omit the delta.content since we don't know how large could that be
				renderGenericToolCall(w, cs, name)

				// If it's JSON, treat it as such, otherwise we skip whatever the content is.
				_ = renderContentAsJSONMarkdown(choice.Delta.Content, w, io)
			}
		}
	}
	return stop, nil
}

func renderContentAsJSONMarkdown(content string, w io.Writer, io *iostreams.IOStreams) error {
	var contentAsJSON any
	if err := json.Unmarshal([]byte(content), &contentAsJSON); err == nil {
		marshaled, err := json.MarshalIndent(contentAsJSON, "", "  ")
		if err == nil {
			content = string(marshaled)
		}

		if err := renderFileContentAsMarkdown("output.json", string(marshaled), w, io); err != nil {
			return fmt.Errorf("failed to render JSON: %w", err)
		}
	}
	return nil
}

func renderRawMarkdown(md string, w io.Writer, io *iostreams.IOStreams) error {
	// Glamour doesn't add leading newlines when content is a complete
	// markdown document. So, we have to add the leading newline.
	paddingFunc := func(s string) string {
		return fmt.Sprintf("\n%s\n\n", s)
	}

	return renderMarkdownWithPadding(md, w, io, paddingFunc)
}

// renderMarkdownWithPadding renders the given markdown string to the given writer.
// If a paddingFunc is provided, the md string is ran through it before
// rendering. This can be used to add newlines before and after the content.
func renderMarkdownWithPadding(md string, w io.Writer, io *iostreams.IOStreams, paddingFunc func(string) string) error {
	rendered, err := markdown.Render(md,
		markdown.WithTheme(io.TerminalTheme()),
		markdown.WithWrap(io.TerminalWidth()),
	)

	if err != nil {
		return fmt.Errorf("failed to render markdown: %w", err)
	}

	rendered = strings.TrimSpace(rendered)
	if paddingFunc != nil {
		rendered = paddingFunc(rendered)
	}

	fmt.Fprint(w, rendered)

	return nil
}

// renderFileContentAsMarkdown renders the given content as markdown
// based on the file extension of the path.
func renderFileContentAsMarkdown(path, content string, w io.Writer, io *iostreams.IOStreams) error {
	parts := strings.Split(path, ".")
	lang := parts[len(parts)-1]
	content = strings.TrimSpace(content)

	if lang == "md" {
		return renderMarkdownWithPadding(content, w, io, nil)
	}

	md := fmt.Sprintf("```%s\n%s\n```", lang, content)
	// Glamour adds leading newlines when content is only a code block,
	// so we only want to add a trailing newline.
	paddingFunc := func(s string) string {
		return fmt.Sprintf("%s\n\n", s)
	}

	return renderMarkdownWithPadding(md, w, io, paddingFunc)
}

func relativePath(absPath string) string {
	relPath := strings.TrimPrefix(absPath, "/home/runner/work/")

	parts := strings.Split(relPath, "/")

	// The last two parts of the path are the
	// repo name and the repo owner.
	// If that's all we have (or less),
	// we return a friendly name "repository".
	if len(parts) > 2 {
		// Drop the repo owner and name, returning the remaining path.
		return strings.Join(parts[2:], "/")
	}
	return "repository"
}

func unmarshal[T any](raw string) *T {
	var t T
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil
	}
	return &t
}

func renderToolCall(w io.Writer, cs *iostreams.ColorScheme, descriptor, title string) {
	if title != "" {
		title = cs.Bold(title)
	}

	if descriptor != "" && title != "" {
		fmt.Fprintf(w, "%s: %s\n", descriptor, title)
	} else if title == "" {
		fmt.Fprintf(w, "%s\n", descriptor)
	} else {
		fmt.Fprintf(w, "%s\n", title)
	}
}

func renderGenericToolCall(w io.Writer, cs *iostreams.ColorScheme, name string) {
	genericToolCallTitles := map[string]string{
		"codeql_checker":                       "Run CodeQL analysis",
		"github-mcp-server-list_issues":        "List issues on GitHub",
		"github-mcp-server-list_pull_requests": "List pull requests on GitHub",
	}

	descriptor, ok := genericToolCallTitles[name]
	if !ok {
		descriptor = fmt.Sprintf("Call to %s", name)
	}

	renderToolCall(w, cs, descriptor, "")
}

type chatCompletionChunkEntry struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Object  string `json:"object"`
	Choices []struct {
		Delta struct {
			ReasoningText string `json:"reasoning_text"`
			Content       string `json:"content"`
			Role          string `json:"role"`
			ToolCalls     []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				Index int    `json:"index"`
				ID    string `json:"id"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
}

type runSetupToolArgs struct {
	Name string `json:"name"`
}

type bashToolArgs struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// type readBashToolArgs struct {
// 	SessionID string `json:"sessionId"`
// }

// type writeBashToolArgs struct {
// 	SessionID string `json:"sessionId"`
// 	Input     string `json:"input"`
// }

// type stopBashToolArgs struct {
// 	SessionID string `json:"sessionId"`
// }

// type asyncBashToolArgs struct {
// 	Command   string `json:"command"`
// 	SessionID string `json:"sessionId"`
// }

// type readAsyncBashToolArgs struct {
// 	SessionID string `json:"sessionId"`
// }

// type stopAsyncBashToolArgs struct {
// 	SessionID string `json:"sessionId"`
// }

type viewToolArgs struct {
	Path string `json:"path"`
}
type thinkToolArgs struct {
	SessionID string `json:"sessionId"`
	Thought   string `json:"thought"`
}

type reportProgressToolArgs struct {
	CommitMessage string `json:"commitMessage"`
	PrDescription string `json:"prDescription"`
}

type createToolArgs struct {
	FileText string `json:"file_text"`
	Path     string `json:"path"`
}

type strReplaceToolArgs struct {
	NewStr string `json:"new_str"`
	OldStr string `json:"old_str"`
	Path   string `json:"path"`
}
