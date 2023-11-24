package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const (
	host     = "localhost"
	port     = 5432
	user     = "postgres"
	password = "root"
	dbname   = "tokped-scraper"
)

var dbURL = fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)

var productsMutex sync.Mutex

type Product struct {
	Name         string
	Description  string
	ImageLink    string
	Price        string
	Rating       string
	MerchantName string
	NavLink      string
}

func main() {
	// Stored all the scraped objects
	var products []Product

	// Disable HTTP/2
	chromeOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-http2", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("headless", false),
		chromedp.Flag("start-fullscreen", false),
	)

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), chromeOpts...)
	defer cancel()

	ctx, cancel = chromedp.NewContext(ctx)
	defer cancel()

	log.Println("Navigating to the target page...")
	var productNodes []*cdp.Node
	err := chromedp.Run(ctx,
		// visit the target page
		chromedp.Navigate("https://www.tokopedia.com/p/handphone-tablet/handphone?page=0"),
		// wait for the page to load
		chromedp.Sleep(5000*time.Millisecond),
		chromedp.KeyEvent(kb.End),
		chromedp.Sleep(5000*time.Millisecond),
		// query all selector
		chromedp.Nodes(".css-bk6tzz", &productNodes, chromedp.ByQueryAll),
	)
	if err != nil {
		log.Fatal("Error during navigation:", err)
	}

	log.Println("Navigation successful. Waiting for the page content...")

	var wg sync.WaitGroup
	errCh := make(chan error, len(productNodes))

	// scraping logic
	for _, node := range productNodes {
		wg.Add(1)
		go func(node *cdp.Node) {
			defer wg.Done()

			// extract data from the product HTML node
			var name, price, imageLink string
			var insideLink string
			err = chromedp.Run(ctx,
				chromedp.Text(".css-20kt3o", &name, chromedp.ByQuery, chromedp.FromNode(node)),
				chromedp.Text(".css-o5uqvq", &price, chromedp.ByQuery, chromedp.FromNode(node)),
				chromedp.AttributeValue(`img[class="success fade"]`, "src", &imageLink, nil, chromedp.FromNode(node)),
				chromedp.AttributeValue(`a[data-testid="lnkProductContainer"]`, "href", &insideLink, nil),
			)

			if err != nil {
				log.Fatal("Error extract data:", err)
				errCh <- err
				return
			}

			insideLink, err := extractAndDecodeURL(insideLink)
			if err != nil {
				log.Fatal("Error encode link:", err)
				errCh <- err
				return
			}

			if err != nil {
				log.Fatal("Error detail product:", err)
			}

			// initialize a new product instance
			// with scraped data
			product := Product{
				Name:      name,
				Price:     price,
				ImageLink: imageLink,
				NavLink:   insideLink,
			}

			productsMutex.Lock()
			products = append(products, product)
			productsMutex.Unlock()
		}(node)
	}

	wg.Wait()

	close(errCh)

	for idxProduct, product := range products {
		var detailProductNodes []*cdp.Node
		err = chromedp.Run(ctx,
			chromedp.Navigate(product.NavLink),
			chromedp.Sleep(5000*time.Millisecond),
			chromedp.Nodes("#main-pdp-container", &detailProductNodes, chromedp.ByQueryAll),
		)

		var description, rating, merchantName string
		for _, detailNode := range detailProductNodes {
			err = chromedp.Run(ctx,
				chromedp.Text(`div[data-testid="lblPDPDescriptionProduk"]`, &description, chromedp.ByQuery, chromedp.FromNode(detailNode)),
				chromedp.Text(`span[data-testid="lblPDPDetailProductRatingNumber"]`, &rating, chromedp.ByQuery, chromedp.FromNode(detailNode)),
				chromedp.Text(`h2[class="css-1wdzqxj-unf-heading e1qvo2ff2"]`, &merchantName, chromedp.ByQuery, chromedp.FromNode(detailNode)),
			)
		}

		products[idxProduct].Description = description
		products[idxProduct].Rating = rating
		products[idxProduct].MerchantName = merchantName
	}

	log.Println(len(products))

	if err := InsertIntoDatabase(products); err != nil {
		log.Fatalln("Failed to insert into database:", err)
	}

	err = WriteProductsToCSV(products, "products.csv")
	if err != nil {
		log.Fatalln("Failed to write to CSV file:", err)
	}
}

func extractAndDecodeURL(inputURL string) (string, error) {
	// Find the index of "r=" in the URL
	index := strings.Index(inputURL, "r=")

	// Extract the substring after "r="
	if index != -1 {
		trimmedURL := inputURL[index+len("r="):]

		// Decode the trimmed URL
		decodedURL, err := url.QueryUnescape(trimmedURL)
		if err != nil {
			return "", err
		}

		return decodedURL, nil
	}

	return "", fmt.Errorf("substring not found in the URL")
}

func WriteProductsToCSV(products []Product, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)

	headers := []string{
		"Name of Product",
		"Description",
		"Image Link",
		"Price",
		"Rating (out of 5 stars)",
		"Name of Store or Merchant",
	}

	if err := writer.Write(headers); err != nil {
		return err
	}

	for _, product := range products {
		record := []string{
			product.Name,
			product.Description,
			product.ImageLink,
			product.Price,
			product.Rating,
			product.MerchantName,
		}

		if err := writer.Write(record); err != nil {
			return err
		}
	}

	writer.Flush()

	if err := writer.Error(); err != nil {
		return err
	}

	return nil
}

func InsertIntoDatabase(products []Product) error {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return err
	}
	defer db.Close()

	for _, product := range products {
		_, err := db.Exec(`
            INSERT INTO products (name, description, image_link, price, rating, merchant_name)
            VALUES ($1, $2, $3, $4, $5, $6)
        `, product.Name, product.Description, product.ImageLink, product.Price, product.Rating, product.MerchantName)

		if err != nil {
			return err
		}
	}

	return nil
}
