package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/silentsokolov/go-vimeo/vimeo"
	"golang.org/x/oauth2"
)

var vimeoAccessToken string
var iconikAppID string
var iconikAuthToken string

func downloadVideo(link, name, mime string) error {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return err
	}

	// Optionally, set custom headers
	req.Header.Set("Authorization", "bearer {"+vimeoAccessToken+"}")

	// Inspect the headers being sent
	for key, value := range req.Header {
		fmt.Printf("header %s: %s\n", key, value)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("received status code %d", resp.StatusCode)
	}
	extension := ""
	if mime == "video/mp4" {
		extension = ".mp4"
	} else if mime == "video/quicktime" {
		extension = ".mov"
	} else {
		return fmt.Errorf("Unsupported video type:", mime)
	}

	outFile, err := os.Create(name + extension)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer outFile.Close()

	_, err = outFile.ReadFrom(resp.Body)
	if err != nil {
		return fmt.Errorf("error writing to file: %w", err)
	}

	fmt.Println("Video downloaded successfully.")
	return nil
}

func main() {
	// flags for the username and password for Vimeo, and also the Iconik APPID and AuthToken
	flag.StringVar(&vimeoAccessToken, "vimeo-access-token", "7057388dadfc5cfa11f44886450bc5d9", "Vimeo access token")
	flag.StringVar(&iconikAppID, "iconik-appid", "", "Iconik App ID")
	flag.StringVar(&iconikAuthToken, "iconik-auth-token", "", "Iconik Auth Token")
	flag.Parse()
	// client identifier: c0513fafcbc871f615c846a8f3718572e774c7a6

	/*	if vimeoUsername == "" || vimeoPassword == "" || iconikAppID == "" || iconikAuthToken == "" {
		fmt.Println("Usage: go run main.go --vimeo-username <username> --vimeo-password <password> --iconik-appid <appid> --iconik-auth-token <auth-token>")
		os.Exit(1)
	} */
	// make a request to Vimeo to get

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: vimeoAccessToken},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)

	client := vimeo.NewClient(tc, nil)

	pageNum := 1
	videos := 0
	for {
		log.Printf("Fetching page %d of videos from Vimeo...\n", pageNum)
		vids, _, err := client.Videos.MyList(vimeo.OptSort("alphabetical"), vimeo.OptDirection("asc"), vimeo.OptPerPage(100), vimeo.OptPage(pageNum))
		if err != nil {
			vids, _, err = client.Videos.MyList(vimeo.OptSort("alphabetical"), vimeo.OptDirection("asc"), vimeo.OptPerPage(100), vimeo.OptPage(pageNum))
			if err != nil {
				fmt.Println("Error fetching videos from Vimeo:", err)
				os.Exit(1)
			}
		}
		pageNum++

		if len(vids) == 0 {
			log.Println("No more videos found.")
			break
		}

		for _, video := range vids {
			sourceSize := 0
			sourceWidth := 0
			quality := ""
			foundSource := false
			//			spew.Dump(video)
			for _, dl := range video.Download {
				if dl.Quality == "source" {
					log.Printf("Video Name: %s, size: %d\n", video.Name, dl.Size) // can we also find out what folder it is in?
					foundSource = true
					videos++
					/*					fmt.Printf("Downloading video from URL: %s\n", dl.Link)
										if err := downloadVideo(dl.Link, video.Name, dl.Type); err == nil {
											log.Printf("Video %s downloaded successfully.\n", video.Name)
										} */
					break
				}
				if dl.Width > sourceWidth {
					quality = dl.Quality
					sourceSize = dl.Size
					sourceWidth = dl.Width
				}
			}
			if !foundSource {
				if sourceSize > 0 {
					log.Printf("Video Name [%s]: %s, size: %d\n", quality, video.Name, sourceSize) // can we also find out what folder it is in?
					videos++
				} else {
					log.Printf("No source quality video found for %s, only had %v\n", video.Name)
				}
			}
		}
	}
	log.Printf("Total videos processed: %d\n", videos)
}
