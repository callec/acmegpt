package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"9fans.net/go/acme"
	"gopkg.in/yaml.v3"
)

const (
	roleMarker = "###"
)

var fileRefRegex = regexp.MustCompile(`^\+file\s+([^:]+)(?::(\d+)(?:,(\d+))?)?$`)

var win *acme.Win
var provider Provider
var ctx context.Context
var needchat = make(chan bool, 1)
var needstop = make(chan bool, 1)

var fileCache = FileCache{
	files: make(map[string]string),
}

type Message struct {
	Role    string
	Content string
}

type config struct {
	Provider     string   `yaml:"provider"`
	Key          string   `yaml:"key"`
	Model        string   `yaml:"model"`
	SystemPrompt []string `yaml:"system_prompt"`
	Debug        bool     `yaml:"debug"`
}

type Provider interface {
	StreamChat(ctx context.Context, message []Message) (io.ReadCloser, error)
	Close() error
}

type FileCache struct {
	mu    sync.RWMutex
	files map[string]string
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: acmegpt\n")
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("acmegpt: ")
	flag.Usage = usage
	flag.Parse()

	model := "acmegpt"

	file := path.Join(os.Getenv("HOME"), ".acmegpt")
	data, err := os.ReadFile(file)
	if err == nil {
		var conf config
		if err := yaml.Unmarshal(data, &conf); err != nil {
			log.Fatalf("unmarshal %s: %v", file, err)
		}
		if !conf.Debug {
			log.SetOutput(io.Discard)
		}
		if conf.Provider == "" {
			log.Fatalf("provider missing in config")
		}
		model = conf.Provider
		provider, err = NewProvider(conf)
		if err != nil {
			log.Fatal(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}

	ctx = context.Background()

	win, err = acme.New()
	if err != nil {
		log.Fatal(err)
	}
	win.Name("+%s", model)
	win.Ctl("clean")
	win.Fprintf("tag", "Get Stop ")
	win.Write("data", []byte(roleMarker+" User\n"))

	go chat()

Events:
	for e := range win.EventChan() {
		if e.C2 == 'x' || e.C2 == 'X' {
			switch string(e.Text) {
			case "Get":
				select {
				case needchat <- true:
				default:
				}
				continue Events
			case "Stop":
				select {
				case needstop <- true:
				default:
				}
				continue Events
			}
		}
		win.WriteEvent(e)
	}
}

func parseFile(line string) (string, []int, error) {
	matches := fileRefRegex.FindStringSubmatch(line)
	if matches == nil {
		return "", nil, fmt.Errorf("invalid file reference: %s", line)
	}

	filePath := matches[1]
	var rows []int

	if matches[2] != "" {
		row1, _ := strconv.Atoi(matches[2])
		rows = append(rows, row1)
		if matches[3] != "" {
			row2, _ := strconv.Atoi(matches[3])
			rows = append(rows, row2)
		}
	}

	return filePath, rows, nil
}

func loadFile(line string) (string, error) {
	filePath, rows, err := parseFile(line)
	if err != nil {
		return "", err
	}

	fileCache.mu.RLock()
	cachedContent, exists := fileCache.files[filePath]
	fileCache.mu.RUnlock()

	var content string
	if !exists {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", err
		}
		content = string(data)

		fileCache.mu.Lock()
		fileCache.files[filePath] = content
		fileCache.mu.Unlock()
	} else {
		log.Printf("cache hit: %s", filePath)
		content = cachedContent
	}

	ext := strings.TrimPrefix(path.Ext(filePath), ".")
	lineRange := ""

	if len(rows) > 0 {
		lines := strings.Split(content, "\n")
		if len(rows) == 1 {
			if rows[0] > len(lines) && rows[0] < 1 {
				return "", fmt.Errorf("illegal line number: %d", rows[0])
			}
			content = lines[rows[0]-1]
			lineRange = fmt.Sprintf(":%d", rows[0])
		} else if len(rows) == 2 {
			if rows[1] > len(lines) && rows[1] < 1 {
				return "", fmt.Errorf("illegal line number: %d", rows[0])
			}
			content = strings.Join(lines[rows[0]-1:rows[1]], "\n")
			lineRange = fmt.Sprintf(":%d,%d", rows[0], rows[1])
		}
	}

	return fmt.Sprintf("```%s %s\n%s\n```", ext, path.Base(filePath)+lineRange, content), nil
}

func chat() {
	for range needchat {
		select {
		case <-needstop:
		default:
		}

		messages := readMessages()
		stream, err := provider.StreamChat(ctx, messages)
		if err != nil {
			log.Printf("chat: %v", err)
			continue
		}

		//		win.Addr("data", "$")
		//		win.Ctl("dot=addr")

		//		text := markdownfmt(resp.Choices[0].Message.Content)
		//		text = strings.Replace(text, "\n", "\n\t", -1)

		win.Addr("$")
		win.Write("data", []byte("\n\n"+roleMarker+" Assistant\n"))
		//		win.Write("data", []byte(text))

	Read:
		for {
			select {
			case <-needstop:
				win.Write("data", []byte("<Stopped by user>\n"))
				break Read
			default:
			}

			buf := make([]byte, 1024)
			n, err := stream.Read(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("chat: %v", err)
				break
			}

			out := strings.Replace(string(buf[:n]), "\n", "\n", -1)
			win.Write("data", []byte(out))
		}
		stream.Close()
		win.Write("data", []byte("\n"+roleMarker+" User\n"))
		win.Ctl("clean")
	}
}

func readMessages() (messages []Message) {
	data, _ := win.ReadAll("body")
	text := string(data)

	usedFiles := make(map[string]bool)

	segments := strings.Split(text, roleMarker)
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		lines := strings.SplitN(segment, "\n", 2)
		role := strings.TrimSpace(lines[0])
		if len(lines) < 2 {
			continue
		}

		content := strings.TrimSpace(lines[1])
		var files []string

		switch role {
		case "Assistant":
			if content != "" {
				messages = append(messages, Message{Role: "assistant", Content: content})
			}
		case "User":
			for strings.HasPrefix(content, "+file") {
				var fileSpec string
				lines = strings.SplitN(content, "\n", 2)
				filePath, _, err := parseFile(strings.TrimSpace(lines[0]))
				if err == nil {
					usedFiles[filePath] = true
				}
				fileSpec, err = loadFile(strings.TrimSpace(lines[0]))
				if err != nil {
					log.Printf("loadFile: %v", err)
					continue
				}
				files = append(files, fileSpec)
				if len(lines) > 1 {
					content = strings.TrimSpace(lines[1])
				} else {
					content = ""
				}
			}
			if content != "" || len(files) > 0 {
				content = join(strings.Join(files, "\n"), content, "\n")
				messages = append(messages, Message{Role: "user", Content: content})
			}
		default:
			log.Printf("unknown role: %s", role)
			if content != "" {
				messages = append(messages, Message{Role: "user", Content: content})
			}
		}
	}

	fileCache.mu.Lock()
	for path := range fileCache.files {
		if !usedFiles[path] {
			log.Printf("unused file: %s", path)
			delete(fileCache.files, path)
		}
	}
	fileCache.mu.Unlock()

	return
}

func join(left, right, delim string) string {
	if len(left) == 0 {
		return right
	}
	if len(right) == 0 {
		return left
	}
	return left + "\n" + right
}

/*
func markdownfmt(text string) string {
	const extensions = blackfriday.EXTENSION_NO_INTRA_EMPHASIS |
		blackfriday.EXTENSION_TABLES |
		blackfriday.EXTENSION_FENCED_CODE |
		blackfriday.EXTENSION_AUTOLINK |
		blackfriday.EXTENSION_STRIKETHROUGH |
		blackfriday.EXTENSION_SPACE_HEADERS |
		blackfriday.EXTENSION_NO_EMPTY_LINE_BEFORE_BLOCK

	return string(blackfriday.Markdown([]byte(text), markdown.NewRenderer(nil), extensions))
}
*/
