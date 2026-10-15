package opencode

import "encoding/json"

const defaultHarnessPrompt = "You are an AI agent running in OpenCode, a coding agent harness. Help the user accomplish their goals using the tools you have available."

const v2ToolsJSON = `[
  {
    "type": "function",
    "function": {
      "name": "edit",
      "description": "Edit the contents of a file by finding and replacing exact text. When editing text from Read output, preserve the exact indentation (tabs or spaces) and omit the line-number prefix, such as '1: '. Never include the prefix in oldString or newString. The edit fails if oldString is not found. By default, oldString must identify a UNIQUE location. Multiple matches FAIL unless replaceAll is true. Add more surrounding context to disambiguate, or set replaceAll to true to replace every occurrence. Use replaceAll when the change should apply to every occurrence, such as renaming a variable.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {
            "type": "string",
            "description": "File to edit"
          },
          "oldString": {
            "type": "string",
            "description": "Exact text to find and replace"
          },
          "newString": {
            "type": "string",
            "description": "Text to replace oldString with (must differ from oldString)"
          },
          "replaceAll": {
            "type": "boolean",
            "description": "Whether to replace every occurrence of oldString. When false, oldString must match exactly once. Defaults to false."
          }
        },
        "required": [
          "path",
          "oldString",
          "newString"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  },
  {
    "type": "function",
    "function": {
      "name": "glob",
      "description": "Search file paths using a glob pattern (examples: \"**/*.ts\", \"src/**/*.tsx\").",
      "parameters": {
        "type": "object",
        "properties": {
          "pattern": {
            "type": "string",
            "description": "Glob pattern to match files against"
          },
          "path": {
            "type": "string",
            "description": "Directory to search. Defaults to the current working directory."
          },
          "hidden": {
            "type": "boolean",
            "description": "Include hidden files and directories (default: false)."
          },
          "limit": {
            "type": "integer",
            "exclusiveMinimum": 0,
            "description": "Maximum number of matching files to return (default: 100)"
          }
        },
        "required": [
          "pattern"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  },
  {
    "type": "function",
    "function": {
      "name": "grep",
      "description": "Search file contents using ripgrep's regular expression syntax or literal text matching. Use it to locate specific code, symbols, or text patterns, and narrow searches with 'path' or 'include'. Returns matching file paths, line numbers, and line previews.",
      "parameters": {
        "type": "object",
        "properties": {
          "pattern": {
            "type": "string",
            "minLength": 1,
            "description": "Regular expression or literal text to match in file contents."
          },
          "path": {
            "type": "string",
            "description": "File or directory to search. Defaults to the current working directory."
          },
          "include": {
            "type": "string",
            "description": "Glob pattern to filter files (for example, \"*.js\" or \"*.{ts,tsx}\")"
          },
          "literal": {
            "type": "boolean",
            "description": "Treat 'pattern' as exact text instead of a regular expression (default: false)."
          },
          "caseSensitive": {
            "type": "boolean",
            "description": "Use case-sensitive matching (default: true)."
          },
          "limit": {
            "type": "integer",
            "exclusiveMinimum": 0,
            "description": "Maximum number of matching lines to return (default: 100)"
          }
        },
        "required": [
          "pattern"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  },
  {
    "type": "function",
    "function": {
      "name": "question",
      "description": "Use this tool when you need to ask the user questions during execution. This allows you to:\n1. Gather user preferences or requirements\n2. Clarify ambiguous instructions\n3. Get decisions on implementation choices as you work\n4. Offer choices to the user about what direction to take.\n\nUsage notes:\n- A \"Type your own answer\" option is added automatically; don't include a separate option for free form answers\n- Set 'multiple: true' to allow selecting more than one option\n- If you recommend a specific option, make that the first option in the list and add \"(Recommended)\" at the end of the label",
      "parameters": {
        "type": "object",
        "properties": {
          "questions": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "question": {
                  "type": "string",
                  "description": "Complete question"
                },
                "header": {
                  "type": "string",
                  "description": "Very short label (max 30 chars)"
                },
                "options": {
                  "type": "array",
                  "items": {
                    "type": "object",
                    "properties": {
                      "label": {
                        "type": "string",
                        "description": "Display text (1-5 words, concise)"
                      },
                      "description": {
                        "type": "string",
                        "description": "Explanation of choice"
                      }
                    },
                    "required": [
                      "label",
                      "description"
                    ],
                    "additionalProperties": false
                  },
                  "description": "Available choices"
                },
                "multiple": {
                  "type": "boolean"
                }
              },
              "required": [
                "question",
                "header",
                "options"
              ],
              "additionalProperties": false
            },
            "minItems": 1,
            "description": "Questions to ask"
          }
        },
        "required": [
          "questions"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  },
  {
    "type": "function",
    "function": {
      "name": "read",
      "description": "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {
            "type": "string",
            "description": "File or directory to read"
          },
          "offset": {
            "type": "integer",
            "minimum": 0,
            "description": "The line or directory entry to start reading from (1-based)"
          },
          "limit": {
            "type": "integer",
            "minimum": 0,
            "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"
          }
        },
        "required": [
          "path"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  },
  {
    "type": "function",
    "function": {
      "name": "shell",
      "description": "Execute a shell command and return its output. Commands run on Linux using zsh. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
      "parameters": {
        "type": "object",
        "properties": {
          "command": {
            "type": "string",
            "description": "Shell command string to execute"
          },
          "workdir": {
            "type": "string",
            "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."
          },
          "timeout": {
            "type": "integer",
            "minimum": 0,
            "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."
          },
          "background": {
            "type": "boolean",
            "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."
          }
        },
        "required": [
          "command"
        ],
        "additionalProperties": false
      },
      "strict": false
    }
  }
]`

var (
	cachedWireTools []wireTool
	cachedRespTools []respWireTool
)

func init() {
	if err := json.Unmarshal([]byte(v2ToolsJSON), &cachedWireTools); err != nil {
		panic(err)
	}
	cachedRespTools = make([]respWireTool, len(cachedWireTools))
	for i, w := range cachedWireTools {
		cachedRespTools[i] = respWireTool{
			Type:        w.Type,
			Name:        w.Function.Name,
			Description: w.Function.Description,
			Parameters:  w.Function.Parameters,
		}
	}
}

func v2GateWireTools() []wireTool {
	return cachedWireTools
}

func v2GateRespTools() []respWireTool {
	return cachedRespTools
}
