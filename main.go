package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	iconik "github.com/jzhang919/iconikclient2"
	"github.com/silentsokolov/go-vimeo/vimeo"
	"golang.org/x/oauth2"
)

var vimeoAccessToken string
var iconikAppID string
var iconikAuthToken string
var iconikRootCollection string
var debugHTTP bool

// collectionCache maps "baseCollectionID::folder/path" → iconik collection UUID
// so that repeated walks of the same path do not make redundant API calls.
var collectionCache map[string]string

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

// sanitizeFilename replaces characters that are problematic in filenames or
// B2 object names with underscores.
func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\x00':
			return '_'
		}
		return r
	}, name)
}

// listAllFolders paginates through all folders for the authenticated user
// (the Vimeo API returns every folder regardless of nesting level).
func listAllFolders(client *vimeo.Client) ([]*vimeo.Folder, error) {
	var all []*vimeo.Folder
	for page := 1; ; page++ {
		folders, resp, err := client.Users.ListFolders(
			"",
			vimeo.OptPerPage(100),
			vimeo.OptPage(page),
		)
		if err != nil {
			return nil, fmt.Errorf("listing folders page %d: %w", page, err)
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
// Extra CallOptions (e.g. vimeo.OptFilter("folder")) are appended after the
// mandatory page/per_page options.
func listAllFolderItems(client *vimeo.Client, folderURI string, extra ...vimeo.CallOption) ([]*vimeo.FolderItem, error) {
	var all []*vimeo.FolderItem
	for page := 1; ; page++ {
		opts := append([]vimeo.CallOption{vimeo.OptPerPage(100), vimeo.OptPage(page)}, extra...)
		items, resp, err := client.Users.ListFolderItems(folderURI, opts...)
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

// findRootFolders identifies which folders in allFolders are true root-level
// folders — those that do not appear as a sub-folder of any other folder.
// It calls listAllFolderItems with filter=folder for each folder, which is
// cheaper than fetching all items (videos are excluded from the response).
func findRootFolders(client *vimeo.Client, allFolders []*vimeo.Folder) ([]*vimeo.Folder, error) {
	childURIs := make(map[string]bool)
	for _, f := range allFolders {
		items, err := listAllFolderItems(client, f.URI, vimeo.OptFilter("folder"))
		if err != nil {
			return nil, fmt.Errorf("listing sub-folders of %q: %w", f.Name, err)
		}
		for _, item := range items {
			if item.Type == "folder" && item.Folder != nil {
				childURIs[item.Folder.URI] = true
			}
		}
	}
	var roots []*vimeo.Folder
	for _, f := range allFolders {
		if !childURIs[f.URI] {
			roots = append(roots, f)
		}
	}
	return roots, nil
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

// findOrCreateSubCollection searches for a collection with the given name that
// is a direct child of parentID. If found, its UUID is returned; otherwise a
// new sub-collection is created and its UUID returned.
func findOrCreateSubCollection(ic *iconik.IClient, parentID, name string) (string, error) {
	results, err := ic.SearchWithTitleAndTag(name, "", true)
	if err != nil {
		return "", fmt.Errorf("searching for collection %q: %w", name, err)
	}
	// Filter to an exact title match whose parent is parentID.
	for _, obj := range results.Objects {
		if obj.Title != name {
			log.Printf("checking title %s against %s\n", obj.Title, name)
			continue
		}
		for _, p := range obj.InCollections {
			log.Printf("checking parent %s against %s\n", p, parentID)
			if p == parentID {
				log.Printf("    Found existing collection %q (id=%s)", name, obj.Id)
				return obj.Id, nil
			}
		}
	}
	// Not found — create it.
	log.Fatalf("done here, had %d\n", len(results.Objects))
	log.Printf("    Creating sub-collection %q under %s", name, parentID)
	id, err := ic.CreateCollection(name, parentID)
	if err != nil {
		return "", fmt.Errorf("creating collection %q: %w", name, err)
	}
	return id, nil
}

// ensureCollectionPath walks the segments of folderPath (split on " / "),
// finding or creating each level of sub-collection under baseCollectionID.
// Results are cached so repeated calls for the same path are cheap.
// Returns the UUID of the deepest (leaf) collection.
func ensureCollectionPath(ic *iconik.IClient, baseCollectionID, folderPath string) (string, error) {
	segments := strings.Split(folderPath, " / ")
	currentID := baseCollectionID
	currentPath := ""
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if currentPath == "" {
			currentPath = seg
		} else {
			currentPath = currentPath + " / " + seg
		}
		cacheKey := baseCollectionID + "::" + currentPath
		if id, ok := collectionCache[cacheKey]; ok {
			currentID = id
			continue
		}
		id, err := findOrCreateSubCollection(ic, currentID, seg)
		if err != nil {
			return "", fmt.Errorf("ensuring collection path %q: %w", currentPath, err)
		}
		collectionCache[cacheKey] = id
		currentID = id
	}
	return currentID, nil
}

// videoAlreadyUploaded returns true if an asset with exactly the given title
// already exists inside collectionID in Iconik AND its completed file size
// matches expectedSize. A partial upload (file record not yet CLOSED) returns
// false even if the title matches, so the upload will be retried.
func videoAlreadyUploaded(ic *iconik.IClient, title string, expectedSize int64, collectionID string) (bool, error) {
	results, err := ic.SearchWithTitleAndTag(title, "", false)
	if err != nil {
		return false, fmt.Errorf("searching for existing asset %q: %w", title, err)
	}
	log.Printf("searched with '%s' and got %d results\n", title, len(results.Objects))
	for _, obj := range results.Objects {
		log.Printf("checking title %s matches %s\n", obj.Title, title)
		if obj.Title != title {
			continue
		}
		for _, c := range obj.InCollections {
			log.Printf("  checking collection %s matches %s\n", c, collectionID)
			if c == collectionID {
				// Title and collection match — verify the upload completed at the right size.
				size, err := ic.GetAssetFileSize(obj.Id)
				if err != nil {
					return false, fmt.Errorf("checking file size for %q: %w", title, err)
				}
				log.Printf("  checking file size %d matches expected %d\n", size, expectedSize)
				if size == expectedSize {
					return true, nil
				}
				// Size 0 means no CLOSED file record — partial upload.
				// Any other value means a different file landed here.
				log.Printf("  Found asset %q but size mismatch (iconik=%d, expected=%d)", title, size, expectedSize)
			}
		}
	}
	return false, nil
}

// uploadSinglePart streams all bytes from body and uploads them to B2 in a
// single request. Used for files at or below the multipart threshold (100 MB).
func uploadSinglePart(NAU *iconik.NewAssetUpload, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("reading source bytes: %w", err)
	}
	hasher := sha1.New()
	hasher.Write(data)
	sha1Hash := fmt.Sprintf("%x", hasher.Sum(nil))

	req, err := http.NewRequest(http.MethodPost, NAU.UploadURL, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("building B2 request: %w", err)
	}
	req.Header.Set("Authorization", NAU.UploadAuthToken)
	req.Header.Set("X-Bz-File-Name", url.PathEscape(NAU.UploadFilename))
	req.Header.Set("X-Bz-Content-Sha1", sha1Hash)
	req.Header.Set("Content-Type", NAU.MimeType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting to B2: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading B2 response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("B2 returned status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// uploadMultipart reads body in 100 MB chunks and uploads each as a numbered
// B2 multipart part. SHA-1 digests collected during upload are stored into
// NAU.Sha1List so that FinishMultipartUpload can complete the session.
func uploadMultipart(NAU *iconik.NewAssetUpload, body io.Reader) error {
	buf := make([]byte, iconik.MULTIPART_FILESIZE_THRESHOLD)
	var shas []string

	for partNum := 1; ; partNum++ {
		n, readErr := io.ReadFull(body, buf)
		if n == 0 {
			break
		}
		chunk := buf[:n]

		hasher := sha1.New()
		hasher.Write(chunk)
		sha1Hash := fmt.Sprintf("%x", hasher.Sum(nil))

		req, err := http.NewRequest(http.MethodPost, NAU.UploadURL, bytes.NewBuffer(chunk))
		if err != nil {
			return fmt.Errorf("building B2 request for part %d: %w", partNum, err)
		}
		req.ContentLength = int64(n)
		req.Header.Set("Authorization", NAU.UploadAuthToken)
		req.Header.Set("X-Bz-Part-Number", fmt.Sprintf("%d", partNum))
		req.Header.Set("X-Bz-Content-Sha1", sha1Hash)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("posting part %d: %w", partNum, err)
		}
		respBody, readBodyErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readBodyErr != nil {
			return fmt.Errorf("reading B2 response for part %d: %w", partNum, readBodyErr)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("B2 returned status %d for part %d: %s", resp.StatusCode, partNum, respBody)
		}

		type partResp struct {
			ContentSha1 string `json:"contentSha1"`
		}
		var pr partResp
		if err := json.Unmarshal(respBody, &pr); err != nil {
			return fmt.Errorf("parsing B2 part response for part %d: %w", partNum, err)
		}
		shas = append(shas, pr.ContentSha1)
		log.Printf("    Uploaded part %d (%s)", partNum, humanSize(n))

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		} else if readErr != nil {
			return fmt.Errorf("reading source for part %d: %w", partNum, readErr)
		}
	}

	NAU.Sha1List = shas
	return nil
}

// transferVideoToIconik creates an Iconik asset for info inside the given
// collection, streams the file from Vimeo's pre-signed URL into a local temp
// file, then uploads that temp file to Backblaze B2 via Iconik-provided
// credentials and finalises the asset.
//
// Saving to the temp file is retried immediately on network error so that a
// mid-stream Vimeo connection drop does not abort the whole transfer.
// If the B2 upload fails, the function waits 10 s then retries using the
// already-downloaded temp file (no second Vimeo fetch needed).
func transferVideoToIconik(ic *iconik.IClient, info videoInfo, collectionID string) error {
	if info.SourceLink == "" {
		return fmt.Errorf("no download URL available for %q", info.Name)
	}

	ext := ".mp4"
	if info.SourceType == "video/quicktime" {
		ext = ".mov"
	}
	fileName := sanitizeFilename(info.Name) + ext
	collectionPrefix := strings.TrimPrefix(iconikRootCollection, "/")
	storagePath := collectionPrefix + "/" + strings.ReplaceAll(info.FolderPath, " / ", "/")

	// Create a temp file to hold the downloaded video.
	tmp, err := os.CreateTemp("", "vimeo-upload-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	// downloadToTemp fetches the video from Vimeo and streams it into tmp,
	// retrying the entire fetch immediately on any network-level error
	// (including mid-stream drops). The temp file is truncated and rewound
	// before each attempt so partial writes don't accumulate.
	downloadToTemp := func() error {
		log.Printf("  Fetching from Vimeo (%s)...", humanSize(info.SourceSize))
		for {
			req, err := http.NewRequest(http.MethodGet, info.SourceLink, nil)
			if err != nil {
				return fmt.Errorf("building Vimeo request: %w", err)
			}
			req.Header.Set("Authorization", "bearer "+vimeoAccessToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Printf("  Vimeo fetch error, retrying immediately: %v", err)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return fmt.Errorf("Vimeo returned status %d for download URL", resp.StatusCode)
			}
			log.Printf("  Fetched response (Content-Length: %d of %d), saving to temp file %s...", resp.ContentLength, info.SourceSize, tmp.Name())
			if err := tmp.Truncate(0); err != nil {
				resp.Body.Close()
				return fmt.Errorf("truncating temp file: %w", err)
			}
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				resp.Body.Close()
				return fmt.Errorf("seeking temp file: %w", err)
			}
			_, copyErr := io.Copy(tmp, resp.Body)
			resp.Body.Close()
			if copyErr != nil {
				log.Printf("  Error saving to temp file, retrying immediately: %v", copyErr)
				continue
			}
			return nil
		}
	}

	if err := downloadToTemp(); err != nil {
		return err
	}
	log.Printf("  Download complete, creating Iconik asset stub...")

	const maxUploadAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxUploadAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("  Upload failed, waiting 10s before retry...")
			time.Sleep(10 * time.Second)
		}

		// Rewind the temp file for this attempt.
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seeking temp file: %w", err)
		}

		NAU, err := ic.MakeNewAsset(
			collectionID,
			fileName,
			info.Name,
			storagePath,
			info.SourceType,
			int64(info.SourceSize),
			info.UploadedAt,
		)
		if err != nil {
			return fmt.Errorf("MakeNewAsset: %w", err)
		}

		multipart := NAU.MultipartFileID != ""
		log.Printf("  Uploading to B2 (multipart=%v)...", multipart)
		if multipart {
			lastErr = uploadMultipart(NAU, tmp)
		} else {
			lastErr = uploadSinglePart(NAU, tmp)
		}
		if lastErr != nil {
			log.Printf("  Upload attempt %d failed: %v", attempt, lastErr)
			continue
		}

		log.Printf("  Finalising asset...")
		if err := ic.FinishUpload(NAU); err != nil {
			return fmt.Errorf("FinishUpload: %w", err)
		}
		return nil
	}
	return fmt.Errorf("upload: %w", lastErr)
}

func main() {
	flag.StringVar(&vimeoAccessToken, "vimeo-access-token", "", "Vimeo access token")
	flag.StringVar(&iconikAppID, "iconik-appid", "", "Iconik App ID")
	flag.StringVar(&iconikAuthToken, "iconik-auth-token", "", "Iconik Auth Token")
	flag.StringVar(&iconikRootCollection, "iconik-collection", "", "Iconik root collection name to copy videos into")
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

	log.Println("Fetching all folders from Vimeo...")
	allFolders, err := listAllFolders(client)
	if err != nil {
		log.Fatalf("Error fetching folders: %v", err)
	}
	log.Printf("Found %d folder(s) total; identifying root level...", len(allFolders))
	roots, err := findRootFolders(client, allFolders)
	if err != nil {
		log.Fatalf("Error finding root folders: %v", err)
	}
	log.Printf("Found %d root folder(s).\n", len(roots))

	for _, folder := range roots {
		log.Printf("root folder  - %s (URI: %s)\n", folder.Name, folder.URI)
	}

	var allVideos []videoInfo
	for _, folder := range roots {
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

	// --- Iconik transfer phase ---
	if iconikAppID == "" || iconikAuthToken == "" || iconikRootCollection == "" {
		log.Println("Iconik credentials not provided (--iconik-appid, --iconik-auth-token, --iconik-collection). Skipping upload.")
		os.Exit(0)
	}

	log.Println("Connecting to Iconik...")
	ic, err := iconik.NewIClient(iconik.Credentials{
		AppID: iconikAppID,
		Token: iconikAuthToken,
	}, "", false)
	if err != nil {
		log.Fatalf("Failed to create Iconik client: %v", err)
	}

	log.Printf("Looking up root collection %q in Iconik...", iconikRootCollection)
	// The user may supply a full path like "/Ministries/International/Vimeo".
	// GetCollectionIDs searches by name, so we pass only the leaf segment, then
	// filter the results to the one whose full path matches.
	collectionPath := strings.TrimPrefix(iconikRootCollection, "/")
	parts := strings.Split(collectionPath, "/")
	leafName := parts[len(parts)-1]

	allResults, err := ic.GetCollectionIDs(leafName)
	if err != nil {
		log.Fatalf("Failed to search for collection: %v", err)
	}
	var matched []*iconik.CollectionResult
	for _, r := range allResults {
		if r.Path == collectionPath {
			matched = append(matched, r)
		}
	}
	switch len(matched) {
	case 0:
		log.Fatalf("No collection found with path %q — create it in Iconik first", collectionPath)
	case 1:
		// exactly one match — good
	default:
		log.Fatalf("Ambiguous: %d collections share the path %q — expected exactly one", len(matched), collectionPath)
	}
	rootCollectionID := matched[0].CollectionID
	log.Printf("Using root collection %q (id=%s)", matched[0].Path, rootCollectionID)
	collectionCache = make(map[string]string)

	log.Printf("Starting transfer of %d videos...", len(allVideos))
	failed := 0
	for i, v := range allVideos {
		log.Printf("[%d/%d] %s  (folder: %s)", i+1, len(allVideos), v.Name, v.FolderPath)

		collectionID, err := ensureCollectionPath(ic, rootCollectionID, v.FolderPath)
		if err != nil {
			log.Printf("  ERROR ensuring collection path: %v — skipping", err)
			failed++
			continue
		}

		exists, err := videoAlreadyUploaded(ic, v.Name, int64(v.SourceSize), collectionID)
		if err != nil {
			log.Printf("  WARNING: could not check for existing asset: %v — proceeding with upload", err)
		} else if exists {
			log.Printf("  Already in Iconik — skipping")
			continue
		}
		if err := transferVideoToIconik(ic, v, collectionID); err != nil {
			log.Printf("  ERROR transferring video: %v — skipping", err)
			failed++
			continue
		}
		log.Printf("  OK")
	}

	if failed > 0 {
		log.Printf("Transfer complete. %d succeeded, %d failed.", len(allVideos)-failed, failed)
	} else {
		log.Printf("Transfer complete. All %d videos transferred successfully.", len(allVideos))
	}
}
