package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type WebPage struct {
	Title   string
	URL     string
	Content string
}
type Queue struct {
	total    int
	mu       sync.Mutex
	messages []string
}

func (q *Queue) Enqueue(url string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = append(q.messages, url)
	q.total++
}

func (q *Queue) Dequeue() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.messages) == 0 {
		return "", false
	}

	url := q.messages[0]

	q.messages = q.messages[1:]

	q.total--

	return url, true
}

func main() {

	args := os.Args[1:]

	var wg sync.WaitGroup

	if len(args) == 0 {
		fmt.Println("go run main.go seedurl")
	}

	url := args[0]

	url = strings.ToLower(url)

	queue := Queue{
		total:    0,
		messages: make([]string, 0),
	}

	queue.Enqueue(url)

	StartWorker(&queue, 2)
	// for i := 1; i <= 5000; i++ {
	// 	wg.Add(1)
	// 	defer wg.Done()

	// 	go StartWorker(&myQueue, i)
	// }

	wg.Wait()
}

func fetchPage(url string) []byte {

	fmt.Printf("fetching url from %v\n", url)

	resp, err := http.Get(url)

	if err != nil {
		fmt.Printf("Error fetching url : %v\n", err)
	}
	defer resp.Body.Close()

	result, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body : %v\n", err)
	}
	return result
}
func WriteToFile(webPage WebPage) {
	err := os.WriteFile(os.TempDir(), []byte(fmt.Sprintf("%v : %v\n", webPage.URL, webPage.Content)), 0644)

	if err != nil {
		log.Fatalf("Cannot open file %v\n", err)
	}
}
func parseHtml(content []byte, queue *Queue) {
	z := html.NewTokenizer(bytes.NewReader(content))

	webPage := WebPage{Title: "", URL: "", Content: ""}

	for {
		if z.Next() == html.ErrorToken {
			if z.Err() == io.EOF {
				break // End of the document
			}
			fmt.Println("Error:", z.Err())
			break
		}

		tt := z.Token()

		if tt.Type == html.StartTagToken {
			if tt.Data == "javascript" || tt.Data == "script" || tt.Data == "style" {
				// Skip script and style tags
				z.Next()
				continue
			}
			if tt.Data == "a" {
				// Extract text content of the anchor tag
				for _, attr := range tt.Attr {
					if attr.Key == "href" {

						if len(attr.Val) > 0 && strings.HasPrefix(attr.Val, "https://") {
							webPage.URL = attr.Val
							queue.Enqueue(webPage.URL)
						}
					}
				}
				if z.Next() == html.TextToken {
					webPage.Title = z.Token().Data
					fmt.Printf("Found url : %v : %v\n", webPage.Title, webPage.URL)
				}
			}
		}
	}
}

func StartWorker(queue *Queue, workerID int) {
	fmt.Printf("starting worker : %v\n", workerID)
	for {
		url, success := queue.Dequeue()

		if success {
			result := fetchPage(url)

			parseHtml(result, queue)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
