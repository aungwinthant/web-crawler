package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	URL "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/net/html"
)

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

	var workers, crawl_size = 2, 500

	var wg sync.WaitGroup

	var DB *DB

	//load ENV
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading env file!")
	}

	mongoURI := os.Getenv("MONGO_HOST")
	worker_count := os.Getenv("WORKER_COUNT")
	size := os.Getenv("SIZE")

	//check format
	args := os.Args[1:]
	if len(args) == 0 {
		log.Fatal("go run main.go seedurl.")
	}

	if len(mongoURI) > 0 {
		DB = InitDB(mongoURI)
		defer DB.Client.Disconnect(context.TODO())
	}

	if len(worker_count) > 0 {
		workers, err = strconv.Atoi(worker_count)
		if err != nil {
			log.Fatal("Invalid WORKER_COUNT")
		}
	}

	if len(worker_count) > 0 {
		crawl_size, err = strconv.Atoi(size)

		if err != nil {
			log.Fatal("Invalid SIZE")
		}
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
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go StartWorker(&queue, i, &crawled, &wg, DB, crawl_size)
	}
	wg.Wait()

	fmt.Printf("Crawled : %v records!\n", crawled.Size())
}

func fetchPage(url string, worker int) []byte {

	//TODO : need to refactor with context for workers
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

	if DB != nil {
		//TODO : is it thread safe?
		DB.Insert(webPage)
	}
}

func StartWorker(queue *Queue, workerID int, crawled *CrawlData, wg *sync.WaitGroup, DB *DB, size int) {
	fmt.Printf("starting worker : %v\n", workerID)

	for crawled.Size() < size {
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
