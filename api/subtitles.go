package api

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elgatito/elementum/config"
	"github.com/elgatito/elementum/osdb"
	"github.com/elgatito/elementum/util"
	"github.com/elgatito/elementum/xbmc"
	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
)

var subLog = logging.MustGetLogger("subtitles")

func appendLocalFilePayloads(playingFile string, payloads *[]osdb.SearchPayload) error {
	file, err := os.Open(playingFile)
	if err != nil {
		return err
	}
	defer file.Close()

	hashPayload := osdb.SearchPayload{}
	if h, err := osdb.HashFile(file); err == nil {
		hashPayload.Hash = h
	}
	if s, err := file.Stat(); err == nil {
		hashPayload.Size = s.Size()
	}

	*payloads = append(*payloads, []osdb.SearchPayload{
		hashPayload,
		{Query: strings.Replace(filepath.Base(playingFile), filepath.Ext(playingFile), "", -1)},
	}...)

	return nil
}

func appendMoviePayloads(labels map[string]string, payloads *[]osdb.SearchPayload) error {
	title := labels["VideoPlayer.OriginalTitle"]
	if title == "" {
		title = labels["VideoPlayer.Title"]
	}
	*payloads = append(*payloads, osdb.SearchPayload{
		Query: fmt.Sprintf("%s %s", title, labels["VideoPlayer.Year"]),
	})
	return nil
}

func appendEpisodePayloads(labels map[string]string, payloads *[]osdb.SearchPayload) error {
	season := -1
	if labels["VideoPlayer.Season"] != "" {
		if s, err := strconv.Atoi(labels["VideoPlayer.Season"]); err == nil {
			season = s
		}
	}
	episode := -1
	if labels["VideoPlayer.Episode"] != "" {
		if e, err := strconv.Atoi(labels["VideoPlayer.Episode"]); err == nil {
			episode = e
		}
	}
	if season >= 0 && episode > 0 {
		searchString := fmt.Sprintf("%s S%02dE%02d", labels["VideoPlayer.TVshowtitle"], season, episode)
		*payloads = append(*payloads, osdb.SearchPayload{
			Query: searchString,
		})
	}
	return nil
}

// SubtitlesIndex ...
func SubtitlesIndex(ctx *gin.Context) {
	q := ctx.Request.URL.Query()
	searchString := q.Get("searchstring")
	languages := strings.Split(q.Get("languages"), ",")
	if !config.Get().OSDBAutoLanguage && config.Get().OSDBLanguage != "" {
		languages = []string{config.Get().OSDBLanguage}
	}

	labels := xbmc.InfoLabels(
		"VideoPlayer.Title",
		"VideoPlayer.OriginalTitle",
		"VideoPlayer.Year",
		"VideoPlayer.TVshowtitle",
		"VideoPlayer.Season",
		"VideoPlayer.Episode",
	)
	playingFile := xbmc.PlayerGetPlayingFile()

	// Check if we are reading a file from Elementum
	if strings.HasPrefix(playingFile, util.GetContextHTTPHost(ctx)) {
		playingFile = strings.Replace(playingFile, util.GetContextHTTPHost(ctx)+"/files", config.Get().DownloadPath, 1)
		playingFile, _ = url.QueryUnescape(playingFile)
	}

	for i, lang := range languages {
		if lang == "Portuguese (Brazil)" {
			languages[i] = "pob"
		} else {
			isoLang := xbmc.ConvertLanguage(lang, xbmc.Iso639_2)
			if isoLang == "gre" {
				isoLang = "ell"
			}
			languages[i] = isoLang
		}
	}

	payloads := []osdb.SearchPayload{}
	if searchString != "" {
		payloads = append(payloads, osdb.SearchPayload{
			Query:     searchString,
			Languages: strings.Join(languages, ","),
		})
	} else {
		if strings.HasPrefix(playingFile, "http://") == false && strings.HasPrefix(playingFile, "https://") == false {
			appendLocalFilePayloads(playingFile, &payloads)
		}

		if labels["VideoPlayer.TVshowtitle"] != "" {
			appendEpisodePayloads(labels, &payloads)
		} else {
			appendMoviePayloads(labels, &payloads)
		}
	}

	for i, payload := range payloads {
		payload.Languages = strings.Join(languages, ",")
		payloads[i] = payload
	}

	subLog.Infof("Subtitles payload: %+v", payloads)

	client, err := osdb.NewClient()
	if err != nil {
		subLog.Error(err)
		ctx.String(200, err.Error())
		return
	}
	if err := client.LogIn(config.Get().OSDBUser, config.Get().OSDBPass, config.Get().OSDBLanguage); err != nil {
		subLog.Error(err)
		ctx.String(200, err.Error())
		return
	}

	items := make(xbmc.ListItems, 0)
	results, _ := client.SearchSubtitles(payloads)
	for _, sub := range results {
		rating, _ := strconv.ParseFloat(sub.SubRating, 64)
		subLang := sub.LanguageName
		if subLang == "Brazilian" {
			subLang = "Portuguese (Brazil)"
		}
		item := &xbmc.ListItem{
			Label:     subLang,
			Label2:    sub.SubFileName,
			Icon:      strconv.Itoa(int((rating / 2) + 0.5)),
			Thumbnail: sub.ISO639,
			Path: URLQuery(URLForXBMC("/subtitle/%s", sub.IDSubtitleFile),
				"file", sub.SubFileName,
				"lang", sub.SubLanguageID,
				"fmt", sub.SubFormat,
				"dl", sub.SubDownloadLink),
			Properties: make(map[string]string),
		}
		if sub.MatchedBy == "moviehash" {
			item.Properties["sync"] = trueType
		}
		if sub.SubHearingImpaired == "1" {
			item.Properties["hearing_imp"] = trueType
		}
		items = append(items, item)
	}

	ctx.JSON(200, xbmc.NewView("", items))
}

// SubtitleGet ...
func SubtitleGet(ctx *gin.Context) {
	q := ctx.Request.URL.Query()
	file := q.Get("file")
	dl := q.Get("dl")

	resp, err := http.Get(dl)
	if err != nil {
		subLog.Error(err)
		ctx.String(200, err.Error())
		return
	}
	defer resp.Body.Close()

	reader, err := gzip.NewReader(resp.Body)
	if err != nil {
		subLog.Error(err)
		ctx.String(200, err.Error())
		return
	}
	defer reader.Close()

	subtitlesPath := filepath.Join(config.Get().DownloadPath, "Subtitles")
	if _, errStat := os.Stat(subtitlesPath); os.IsNotExist(errStat) {
		if errMk := os.Mkdir(subtitlesPath, 0755); errMk != nil {
			subLog.Error("Unable to create Subtitles folder")
		}
	}

	outFile, err := os.Create(filepath.Join(subtitlesPath, file))
	if err != nil {
		subLog.Error(err)
		ctx.String(200, err.Error())
		return
	}
	defer outFile.Close()

	io.Copy(outFile, reader)

	ctx.JSON(200, xbmc.NewView("", xbmc.ListItems{
		{Label: file, Path: outFile.Name()},
	}))
}
