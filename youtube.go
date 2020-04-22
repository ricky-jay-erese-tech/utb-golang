package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/net/proxy"
)

//SetLogOutput :Set logger writer
func SetLogOutput(w io.Writer) {
	log.SetOutput(w)
}

//NewYoutube :Initialize youtube package object
func NewYoutube(debug bool) *Youtube {
	return &Youtube{DebugMode: debug, DownloadPercent: make(chan int64, 100)}
}

func NewYoutubeWithSocks5Proxy(debug bool, socks5Proxy string) *Youtube {
	return &Youtube{DebugMode: debug, DownloadPercent: make(chan int64, 100), Socks5Proxy: socks5Proxy}
}

type stream map[string]string

type Youtube struct {
	DebugMode         bool
	StreamList        []stream
	VideoID           string
	videoInfo         string
	DownloadPercent   chan int64
	Socks5Proxy       string
	contentLength     float64
	totalWrittenBytes float64
	downloadLevel     float64
}

//DecodeURL : Decode youtube URL to retrieval video information.
func (y *Youtube) DecodeURL(url string) error {
	err := y.findVideoID(url)
	if err != nil {
		return fmt.Errorf("findVideoID error=%s", err)
	}

	err = y.getVideoInfo()
	if err != nil {
		return fmt.Errorf("getVideoInfo error=%s", err)
	}

	err = y.parseVideoInfo()
	if err != nil {
		return fmt.Errorf("parse video info failed, err=%s", err)
	}

	return nil
}

//StartDownload : Starting download video to specific address.
func (y *Youtube) StartDownload(destFile string) error {
	//download highest resolution on [0]
	err := errors.New("Empty stream list")
	for _, v := range y.StreamList {
		url := v["url"]
		y.log(fmt.Sprintln("Download url=", url))

		y.log(fmt.Sprintln("Download to file=", destFile))
		err = y.videoDLWorker(destFile, url)
		if err == nil {
			break
		}
	}
	return err
}

//StartDownloadWithQuality : Starting download video with specific quality.
func (y *Youtube) StartDownloadWithQuality(destFile string, quality string) error {
	//download highest resolution on [0]
	err := errors.New("Empty stream list")
	for _, v := range y.StreamList {
		if strings.Compare(v["quality"], quality) == 0 {
			url := v["url"]
			y.log(fmt.Sprintln("Download url=", url))
			y.log(fmt.Sprintln("Download to file=", destFile))
			err = y.videoDLWorker(destFile, url)
			if err == nil {
				break
			}
		}
	}

	if err != nil {
		return y.StartDownload(destFile)
	}
	return err
}

//StartDownloadFile : Starting download video on my download.
func (y *Youtube) StartDownloadFile() error {
	//download highest resolution on [0]
	err := errors.New("Empty stream list")
	for _, stream := range y.StreamList {
		streamUrl := stream["url"]
		streamType := stream["type"]
		y.log(fmt.Sprintln("Download url=", streamUrl))

		// Find out what the file name should be.
		fileName := sanitizeFilename(stream["title"])

		// Find out what the file extension should be.
		fileExtensions, err := mime.ExtensionsByType(streamType)
		if err != nil {
			fileName += ".mov"
		} else {
			fileName += fileExtensions[0]
		}

		usr, _ := user.Current()
		destFile := filepath.Join(filepath.Join(usr.HomeDir, "Movies", "youtubedr"), fileName)
		y.log(fmt.Sprintln("Download to file=", destFile))

		err = y.videoDLWorker(destFile, streamUrl)
		if err == nil {
			return nil
		}
	}
	return err
}

func sanitizeFilename(fileName string) string {
	// Characters not allowed on mac
	//	:/
	// Characters not allowed on linux
	//	/
	// Characters not allowed on windows
	//	<>:"/\|?*

	// Ref https://docs.microsoft.com/en-us/windows/win32/fileio/naming-a-file#naming-conventions

	reg, err := regexp.Compile(`[:/<>\:"\\|?*]`)
	if err != nil {
		log.Fatal(err)
	}

	fileName = reg.ReplaceAllString(fileName, "")
	fileName = strings.ReplaceAll(fileName, "  ", " ")
	fileName = strings.ReplaceAll(fileName, "  ", " ")

	return fileName
}

func (y *Youtube) parseVideoInfo() error {
	answer, err := url.ParseQuery(y.videoInfo)
	if err != nil {
		return err
	}

	status, ok := answer["status"]
	if !ok {
		err = fmt.Errorf("no response status found in the server's answer")
		return err
	}
	if status[0] == "fail" {
		reason, ok := answer["reason"]
		if ok {
			err = fmt.Errorf("'fail' response status found in the server's answer, reason: '%s'", reason[0])
		} else {
			err = errors.New(fmt.Sprint("'fail' response status found in the server's answer, no reason given"))
		}
		return err
	}
	if status[0] != "ok" {
		err = fmt.Errorf("non-success response status found in the server's answer (status: '%s')", status)
		return err
	}

	// read the streams map
	streamMap, ok := answer["player_response"]
	if !ok {
		err = errors.New(fmt.Sprint("no stream map found in the server's answer."))
		return err
	}

	// Get video title and author.
	title, author := getVideoTitleAuthor(answer)

	var prData PlayerResponseData
	if err := json.Unmarshal([]byte(streamMap[0]), &prData); err != nil {
		fmt.Println(err)
		panic("Player response json data has changed.")
	}

	// Get video download link
	if prData.PlayabilityStatus.Status == "UNPLAYABLE" {
		//Cannot playback on embedded video screen, could not download.
		return errors.New(fmt.Sprint("Cannot playback and download, reason:", prData.PlayabilityStatus.Reason))
	}

	var streams []stream
	for streamPos, streamRaw := range prData.StreamingData.Formats {
		if streamRaw.MimeType == "" {
			y.log(fmt.Sprintf("An error occured while decoding one of the video's stream's information: stream %d.\n", streamPos))
			continue
		}
		streamUrl := streamRaw.URL
		if streamUrl == "" {
			cipher := streamRaw.Cipher
			decipheredUrl, err := y.decipher(cipher)
			if err != nil {
				return err
			}
			streamUrl = decipheredUrl
		}

		streams = append(streams, stream{
			"quality": streamRaw.Quality,
			"type":    streamRaw.MimeType,
			"url":     streamUrl,

			"title":  title,
			"author": author,
		})
		y.log(fmt.Sprintf("Title: %s Author: %s Stream found: quality '%s', format '%s'", title, author, streamRaw.Quality, streamRaw.MimeType))
	}

	y.StreamList = streams
	if len(y.StreamList) == 0 {
		return errors.New(fmt.Sprint("no stream list found in the server's answer"))
	}
	return nil
}

func (y *Youtube) getHTTPClient() (*http.Client, error) {
	// setup a http client
	httpTransport := &http.Transport{}
	httpClient := &http.Client{Transport: httpTransport}

	if len(y.Socks5Proxy) == 0 {
		y.log("Using http without proxy.")
		return httpClient, nil
	}

	dialer, err := proxy.SOCKS5("tcp", y.Socks5Proxy, nil, proxy.Direct)
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't connect to the proxy:", err)
		return nil, err
	}
	// set our socks5 as the dialer
	httpTransport.Dial = dialer.Dial

	y.log(fmt.Sprintf("Using http with proxy %s.", y.Socks5Proxy))

	return httpClient, nil
}

func (y *Youtube) getVideoInfo() error {
	eurl := "https://youtube.googleapis.com/v/" + y.VideoID
	url := "https://youtube.com/get_video_info?video_id=" + y.VideoID + "&eurl=" + eurl
	y.log(fmt.Sprintf("url: %s", url))

	httpClient, err := y.getHTTPClient()
	if err != nil {
		return err
	}

	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	y.videoInfo = string(body)
	return nil
}

func (y *Youtube) findVideoID(url string) error {
	videoID := url
	if strings.Contains(videoID, "youtu") || strings.ContainsAny(videoID, "\"?&/<%=") {
		reList := []*regexp.Regexp{
			regexp.MustCompile(`(?:v|embed|watch\?v)(?:=|/)([^"&?/=%]{11})`),
			regexp.MustCompile(`(?:=|/)([^"&?/=%]{11})`),
			regexp.MustCompile(`([^"&?/=%]{11})`),
		}
		for _, re := range reList {
			if isMatch := re.MatchString(videoID); isMatch {
				subs := re.FindStringSubmatch(videoID)
				videoID = subs[1]
			}
		}
	}
	y.log(fmt.Sprintf("Found video id: '%s'", videoID))
	y.VideoID = videoID
	if strings.ContainsAny(videoID, "?&/<%=") {
		return errors.New("invalid characters in video id")
	}
	if len(videoID) < 10 {
		return errors.New("the video id must be at least 10 characters long")
	}
	return nil
}

func (y *Youtube) Write(p []byte) (n int, err error) {
	n = len(p)
	y.totalWrittenBytes = y.totalWrittenBytes + float64(n)
	currentPercent := ((y.totalWrittenBytes / y.contentLength) * 100)
	if (y.downloadLevel <= currentPercent) && (y.downloadLevel < 100) {
		y.downloadLevel++
		y.DownloadPercent <- int64(y.downloadLevel)
	}
	return
}
func (y *Youtube) videoDLWorker(destFile string, target string) error {

	httpClient, err := y.getHTTPClient()
	if err != nil {
		return err
	}

	resp, err := httpClient.Get(target)
	if err != nil {
		y.log(fmt.Sprintf("Http.Get\nerror: %s\ntarget: %s\n", err, target))
		return err
	}
	defer resp.Body.Close()
	y.contentLength = float64(resp.ContentLength)

	if resp.StatusCode != 200 {
		y.log(fmt.Sprintf("reading answer: non 200[code=%v] status code received: '%v'", resp.StatusCode, err))
		return errors.New("non 200 status code received")
	}
	err = os.MkdirAll(filepath.Dir(destFile), 0755)
	if err != nil {
		return err
	}
	out, err := os.Create(destFile)
	if err != nil {
		return err
	}
	mw := io.MultiWriter(out, y)
	_, err = io.Copy(mw, resp.Body)
	if err != nil {
		y.log(fmt.Sprintln("download video err=", err))
		return err
	}
	return nil
}

func (y *Youtube) log(logText string) {
	if y.DebugMode {
		log.Println(logText)
	}
}

func getVideoTitleAuthor(in url.Values) (string, string) {
	playResponse, ok := in["player_response"]
	if !ok {
		return "", ""
	}
	personMap := make(map[string]interface{})

	if err := json.Unmarshal([]byte(playResponse[0]), &personMap); err != nil {
		panic(err)
	}

	s := personMap["videoDetails"]
	myMap := s.(map[string]interface{})
	// fmt.Println("-->", myMap["title"], "oooo:", myMap["author"])
	if title, ok := myMap["title"]; ok {
		if author, ok := myMap["author"]; ok {
			return title.(string), author.(string)
		}
	}

	return "", ""
}
