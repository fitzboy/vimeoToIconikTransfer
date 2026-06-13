package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/silentsokolov/go-vimeo/vimeo"
	"golang.org/x/oauth2"
)

var vimeoAccessToken string
var iconikAppID string
var iconikAuthToken string
var debugHTTP bool

// debugTransport wraps an http.RoundTripper and logs the outgoing
// Authorization header for every request. It is injected as the *base*
// transport inside the oauth2 wrapper, so the header has already been added
// by the oauth2 layer by the time we see it here.
type debugTransport struct{ base http.RoundTripper }

func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := req.Header.Get("Authorization")
	if auth == "" {
		log.Printf("DEBUG %s %s  →  Authorization header: <not set>", req.Method, req.URL)
	} else {
		// Show token type and first 8 chars only so it's identifiable but not fully exposed.
		preview := auth
		if len(preview) > 16 {
			preview = preview[:16] + "…"
		}
		log.Printf("DEBUG %s %s  →  Authorization: %s", req.Method, req.URL, preview)
	}
	return t.base.RoundTrip(req)
}

// videoInfo holds all metadata collected for a single video.
type videoInfo struct {
	FolderPath    string
	Name          string
	UploadedAt    time.Time
	SourceSize    int
	SourceLink    string
	SourceType    string
	SourceQuality string // "source" if original file available, otherwise best available quality
	VideoURI      string
}

// extractSourceInfo returns the best available download entry for a video.
// Prefers "source" quality (the original upload); falls back to the
// highest-resolution rendition otherwise.
func extractSourceInfo(video *vimeo.Video) (link, mimeType string, size int, quality string) {
	bestWidth := 0
	for _, dl := range video.Download {
		if dl.Quality == "source" {
			return dl.Link, dl.Type, dl.Size, "source"
		}
		if dl.Width > bestWidth {
			bestWidth = dl.Width
			link = dl.Link
			mimeType = dl.Type
			size = dl.Size
			quality = dl.Quality
		}
	}
	return
}

// humanSize formats a byte count as a human-readable string (e.g. "1.4 GB").
func humanSize(bytes int) string {
	if bytes <= 0 {
		return "unknown"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := unit, 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// downloadVideo downloads a video file from link to disk as name+extension.
// Vimeo download links are pre-signed, but we include the bearer token as a
// fallback. This function is unused in the current inventory phase but is
// kept ready for the forthcoming download step.
func downloadVideo(link, name, mime string) error {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+vimeoAccessToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("received status code %d", resp.StatusCode)
	}

	var extension string
	switch mime {
	case "video/mp4":
		extension = ".mp4"
	case "video/quicktime":
		extension = ".mov"
	default:
		return fmt.Errorf("unsupported video type: %s", mime)
	}

	outFile, err := os.Create(name + extension)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer outFile.Close()

	if _, err = io.Copy(outFile, resp.Body); err != nil {
		return fmt.Errorf("error writing to file: %w", err)
	}

	return nil
}

// listAllRootFolders paginates through all root-level folders for the
// authenticated user, stopping when the API signals no next page.
func listAllRootFolders(client *vimeo.Client) ([]*vimeo.Folder, error) {
	var all []*vimeo.Folder
	for page := 1; ; page++ {
		folders, resp, err := client.Users.ListFolders(
			"",
			vimeo.OptPerPage(100),
			vimeo.OptPage(page),
		)
		if err != nil {
			return nil, fmt.Errorf("listing root folders page %d: %w", page, err)
		}
		all = append(all, folders...)
		if resp.NextPage == "" {
			break
		}
	}
	return all, nil
}

// listAllFolderVideos paginates through all videos in a folder,
// stopping when the API signals no next page.
func listAllFolderVideos(client *vimeo.Client, folderURI string) ([]*vimeo.Video, error) {
	var all []*vimeo.Video
	for page := 1; ; page++ {
		vids, resp, err := client.Users.ListFolderVideos(
			folderURI,
			vimeo.OptSort("date"),
			vimeo.OptDirection("asc"),
			vimeo.OptPerPage(100),
			vimeo.OptPage(page),
		)
		if err != nil {
			return nil, fmt.Errorf("listing videos in %s page %d: %w", folderURI, page, err)
		}
		all = append(all, vids...)
		if resp.NextPage == "" {
			break
		}
	}
	return all, nil
}

// listAllFolderItems paginates through all items (videos + sub-folders) in a
// folder via the /items endpoint, stopping when the API signals no next page.
func listAllFolderItems(client *vimeo.Client, folderURI string) ([]*vimeo.FolderItem, error) {
	var all []*vimeo.FolderItem
	for page := 1; ; page++ {
		items, resp, err := client.Users.ListFolderItems(
			folderURI,
			vimeo.OptPerPage(100),
			vimeo.OptPage(page),
		)
		if err != nil {
			return nil, fmt.Errorf("listing items in %s page %d: %w", folderURI, page, err)
		}
		all = append(all, items...)
		if resp.NextPage == "" {
			break
		}
	}
	return all, nil
}

// walkFolder recursively traverses a folder and all its sub-folders,
// appending a videoInfo entry for every video found.
//
// Videos are fetched via {folderURI}/videos (returns full download metadata).
// Sub-folders are discovered via {folderURI}/items filtered to type "folder".
func walkFolder(client *vimeo.Client, folder *vimeo.Folder, parentPath string, results *[]videoInfo) error {
	var currentPath string
	if parentPath == "" {
		currentPath = folder.Name
	} else {
		currentPath = parentPath + " / " + folder.Name
	}

	log.Printf("Walking folder: %s", currentPath)

	// Collect videos in this folder (full metadata including download links).
	videos, err := listAllFolderVideos(client, folder.URI)
	if err != nil {
		return err
	}
	for _, video := range videos {
		link, mimeType, size, quality := extractSourceInfo(video)
		if quality == "" {
			log.Printf("  WARNING: no downloadable renditions for %q (%s)", video.Name, video.URI)
		}
		*results = append(*results, videoInfo{
			FolderPath:    currentPath,
			Name:          video.Name,
			UploadedAt:    video.CreatedTime,
			SourceSize:    size,
			SourceLink:    link,
			SourceType:    mimeType,
			SourceQuality: quality,
			VideoURI:      video.URI,
		})
	}

	// Discover sub-folders via the /items endpoint (the /folders sub-path
	// does not exist in Vimeo's API).
	items, err := listAllFolderItems(client, folder.URI)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Type == "folder" && item.Folder != nil {
			if err := walkFolder(client, item.Folder, currentPath, results); err != nil {
				return err
			}
		}
	}

	return nil
}

func main() {
	flag.StringVar(&vimeoAccessToken, "vimeo-access-token", "", "Vimeo access token")
	flag.StringVar(&iconikAppID, "iconik-appid", "", "Iconik App ID")
	flag.StringVar(&iconikAuthToken, "iconik-auth-token", "", "Iconik Auth Token")
	flag.BoolVar(&debugHTTP, "debug", false, "Log outgoing HTTP Authorization headers")
	flag.Parse()

	if vimeoAccessToken == "" {
		log.Fatal("--vimeo-access-token is required")
	}

	// Build the base HTTP transport. When --debug is set, wrap it so we can
	// see the Authorization header that the oauth2 layer injects.
	var baseTransport http.RoundTripper = http.DefaultTransport
	if debugHTTP {
		baseTransport = &debugTransport{base: http.DefaultTransport}
	}

	// Inject our (optionally debug-wrapped) transport as the base inside the
	// oauth2 client, so oauth2 adds its Bearer header on top of it.
	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient,
		&http.Client{Transport: baseTransport})

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: vimeoAccessToken},
	)
	tc := oauth2.NewClient(baseCtx, ts)

	client := vimeo.NewClient(tc, nil)

	log.Println("Fetching root folders from Vimeo Team Library...")
	rootFolders, err := listAllRootFolders(client)
	if err != nil {
		log.Fatalf("Error fetching root folders: %v", err)
	}
	log.Printf("Found %d root folder(s).\n", len(rootFolders))

	var allVideos []videoInfo
	for _, folder := range rootFolders {
		if err := walkFolder(client, folder, "", &allVideos); err != nil {
			log.Printf("Warning: error walking folder %q: %v", folder.Name, err)
		}
	}

	// Print the inventory to stdout.
	fmt.Println()
	fmt.Println("=== Vimeo Team Library — Video Inventory ===")
	fmt.Println()
	totalBytes := 0

	for i, v := range allVideos {
		totalBytes += v.SourceSize
		qualityNote := ""
		if v.SourceQuality != "source" && v.SourceQuality != "" {
			qualityNote = fmt.Sprintf(" [best available: %s]", v.SourceQuality)
		}
		fmt.Printf("%4d. %s\n", i+1, v.Name)
		fmt.Printf("      Folder:   %s\n", v.FolderPath)
		fmt.Printf("      Uploaded: %s\n", v.UploadedAt.Format("2006-01-02"))
		fmt.Printf("      Size:     %s%s\n", humanSize(v.SourceSize), qualityNote)
		fmt.Println()
	}
	fmt.Printf("=== Total: %d videos ===\n", len(allVideos))
	fmt.Printf("=== Total size: %s ===\n", humanSize(totalBytes))
}
