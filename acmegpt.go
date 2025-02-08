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
	"strings"

	"9fans.net/go/acme"
	"gopkg.in/yaml.v3"
)

var win *acme.Win
var provider Provider
var ctx context.Context
var needchat = make(chan bool, 1)
var needstop = make(chan bool, 1)

type Message struct {
	Role    string
	Content string
}

type config struct {
	Provider string `yaml:"provider"`
	Key      string `yaml:"key"`
	Model    string `yaml:"model"`
}

type Provider interface {
	StreamChat(ctx context.Context, message []Message) (io.ReadCloser, error)
	Close() error
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
	win.Name(fmt.Sprintf("+%s", model))
	win.Ctl("clean")
	win.Fprintf("tag", "Get Stop ")

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
		win.Write("data", []byte("\n\n\t"))
		//		win.Write("data", []byte(text))

	Read:
		for {
			select {
			case <-needstop:
				win.Write("data", []byte("<Stopped by user>"))
				break Read
			default:
			}

			buf := make([]byte, 1024)
			n, err := stream.Read(buf)
			if err == io.EOF {
				win.Write("data", []byte("\n"))
				break
			}
			if err != nil {
				log.Printf("chat: %v", err)
				break
			}

			out := strings.Replace(string(buf[:n]), "\n", "\n\t", -1)
			win.Write("data", []byte(out))
		}
		stream.Close()

		win.Write("data", []byte("\n"))
		win.Ctl("clean")
	}
}

func readMessages() (messages []Message) {
	data, _ := win.ReadAll("body")
	// Segment body by indentation.
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		role := "user"
		if line[0] == '\t' {
			role = "assistant"
			line = line[1:]
		}
		if len(messages) == 0 || messages[len(messages)-1].Role != role {
			messages = append(messages, Message{
				Role: role,
			})
		}
		m := &messages[len(messages)-1]
		m.Content = join(m.Content, line, "\n")
	}
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
