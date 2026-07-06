// Command reaper deletes DigitalOcean droplets tagged "forge-e2e" older than a
// threshold (default 60m). It is the safety net for e2e runs that crash or are
// cancelled before tearing down their droplet. Run on a cron in CI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/digitalocean/godo"
)

func main() {
	maxAge := flag.Duration("max-age", 60*time.Minute, "delete droplets older than this")
	flag.Parse()

	token := os.Getenv("DIGITALOCEAN_TOKEN")
	if token == "" {
		log.Fatal("DIGITALOCEAN_TOKEN not set")
	}
	client := godo.NewFromToken(token)
	ctx := context.Background()

	opts := &godo.ListOptions{PerPage: 200}
	for {
		droplets, resp, err := client.Droplets.ListByTag(ctx, "forge-e2e", opts)
		if err != nil {
			log.Fatalf("list droplets: %v", err)
		}
		for _, d := range droplets {
			created, err := time.Parse(time.RFC3339, d.Created)
			if err != nil {
				log.Printf("warning: droplet %d bad created time %q: %v", d.ID, d.Created, err)
				continue
			}
			age := time.Since(created)
			if age < *maxAge {
				continue
			}
			if _, err := client.Droplets.Delete(ctx, d.ID); err != nil {
				log.Printf("warning: delete droplet %d (%s): %v", d.ID, d.Name, err)
				continue
			}
			fmt.Printf("deleted droplet %d (%s, age %s)\n", d.ID, d.Name, age.Truncate(time.Minute))
		}
		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		opts.Page, _ = resp.Links.CurrentPage()
		opts.Page++
	}
}
