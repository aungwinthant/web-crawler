package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	URL "net/url"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/net/html"
)

var pages = make([]WebPage, 10)

type WebPage struct {
	Title     string
	URL       string
	Content   string
	CrawledAt int64
}
type Queue struct {
	total    int
	mu       sync.Mutex
	messages []string
}

type CrawlData struct {
	data map[uint64]bool
	mu   sync.Mutex
}

func (c *CrawlData) Add(url string) {
	c.mu.Lock()

	defer c.mu.Unlock()

	c.data[getHash(url)] = true
}

func (c *CrawlData) Size() int {
	c.mu.Lock()

	defer c.mu.Unlock()

	return len(c.data)
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
func getHash(url string) uint64 {
	bytes, err := json.Marshal(url)

	if err != nil {
		return 0
	}

	h := fnv.New64a()
	h.Write(bytes)
	return h.Sum64()
}
func (c *CrawlData) IsVisited(url string) bool {

	hash := getHash(url)

	c.mu.Lock()
	defer c.mu.Unlock()

	_, found := c.data[hash]
	return found
}

type DB struct {
	Client     *mongo.Client
	Collection *mongo.Collection
}

func InitDB(uri string) *DB {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := options.Client().ApplyURI(uri)

	client, err := mongo.Connect(opts)
	if err != nil {
		panic(err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		panic(fmt.Errorf("could not ping mongodb: %w", err))
	}
	fmt.Println("Successfully connected to MongoDB")
	return &DB{
		Client:     client,
		Collection: client.Database("Crawler").Collection("WebPage"),
	}

}

func (DB *DB) Insert(webpage WebPage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	DB.Collection.InsertOne(ctx, webpage)
}
func main() {

	err := godotenv.Load()

	if err != nil {
		fmt.Println("Error loading env file!")
	}

	var DB *DB

	mongoURI := os.Getenv("MONGO_HOST")

	if len(mongoURI) > 0 {
		DB = InitDB(mongoURI)
	}

	args := os.Args[1:]

	var wg sync.WaitGroup

	if len(args) == 0 {
		fmt.Println("go run main.go seedurl.")
	}

	url := args[0]

	url = strings.ToLower(url)

	queue := Queue{
		total:    0,
		messages: make([]string, 0),
	}

	crawled := CrawlData{
		data: make(map[uint64]bool, 1024),
	}
	queue.Enqueue(url)

	crawled.data[getHash(url)] = true

	// StartWorker(&queue, 2, &crawled)
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go StartWorker(&queue, i, &crawled, &wg, DB)
	}

	wg.Wait()

	fmt.Printf("Crawled : %v records!\n", crawled.Size())

	for _, page := range pages {
		fmt.Printf("%v : \t%v\n %v\n\n", page.URL, page.Title, page.Content)
	}
}

func fetchPage(url string, worker int) []byte {

	fmt.Printf("worker #%v fetching url from %v\n", worker, url)

	resp, err := http.Get(url)

	if err != nil {
		fmt.Printf("Error fetching url : %v\n", err)
		return []byte{}
	}
	defer resp.Body.Close()

	result, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body : %v\n", err)
		return []byte{}
	}
	return result
}

func parseHtml(url string, content []byte, queue *Queue, crawled *CrawlData, DB *DB) {
	z := html.NewTokenizer(bytes.NewReader(content))

	webPage := WebPage{Title: "", URL: url, Content: ""}

	baseurl, _ := URL.Parse(url)

	var body, title bool

	var bodyContent bytes.Buffer
	for {

		if z.Next() == html.ErrorToken {
			if z.Err() == io.EOF {
				break // End of the document
			}
			fmt.Println("Error:", z.Err())
			break
		}

		tt := z.Token()

		switch tt.Type {
		case html.StartTagToken:
			if tt.Data == "javascript" || tt.Data == "script" || tt.Data == "style" {
				// Skip script and style tags
				z.Next()
				continue
			}

			if tt.Data == "title" {
				title = true
				continue
			}

			if tt.Data == "p" {
				body = true
				continue
			}
			if tt.Data == "a" {
				// Extract text content of the anchor tag
				for _, attr := range tt.Attr {
					if attr.Key == "href" {

						url = attr.Val

						if len(url) > 0 && (strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "/")) {

							if strings.HasPrefix(url, "/") {
								url = baseurl.Host + url
							}

							visited := crawled.IsVisited(url)

							if visited {
								continue
							} else {
								crawled.Add(url)
								queue.Enqueue(url)
							}
						}
					}
				}
			}
		case html.TextToken:

			if title {
				webPage.Title = strings.TrimSpace(tt.Data)
				title = false
			}
			if body {
				bodyContent.WriteString(strings.TrimSpace(tt.Data))
			}
		case html.EndTagToken:
			if tt.Data == "p" {
				body = false
				webPage.Content = strings.TrimSpace(bodyContent.String())

			}
		}
	}

	webPage.CrawledAt = time.Now().UnixMilli()
	DB.Insert(webPage)
}

func StartWorker(queue *Queue, workerID int, crawled *CrawlData, wg *sync.WaitGroup, DB *DB) {
	fmt.Printf("starting worker : %v\n", workerID)

	for crawled.Size() < 500 {
		url, success := queue.Dequeue()

		if success {
			result := fetchPage(url, workerID)

			if len(result) > 0 {
				parseHtml(url, result, queue, crawled, DB)
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	defer wg.Done()

	fmt.Printf("Worker %v finished and shutting down.\n", workerID)
}
