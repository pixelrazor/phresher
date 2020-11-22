package main

import (
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/patrickmn/go-cache"
	"golang.org/x/oauth2"

	"github.com/zmb3/spotify"
)

const (
	//redirectURI     = "https://phresher.pixelrazor.tech/callback"
	redirectURI = "http://localhost:8086/callback"
	port        = "8086"

	sessionCookieID = "session_id"
	playlistIDKKey  = "playlist_id"
	weeksKey        = "weeks"
	privateKey      = "private"
)

var (
	auth = spotify.NewAuthenticator(redirectURI,
		spotify.ScopePlaylistReadCollaborative,
		spotify.ScopePlaylistReadPrivate,
		spotify.ScopePlaylistModifyPrivate,
		spotify.ScopePlaylistModifyPublic)

	state     string
	authCache *cache.Cache
) // TODO: add greeting to main page after login

/*
"/", "/index.html"	-> home page
					-> if user is authenticated, it will allow them to pick a playlist and a duration
					-> use a form to do a post, with playlists in a dropdown box. Checkbox for private/public
					-> if user is not authenticated, there will be a login button

"/callback" 		-> oauth callback

"/do-the-roar" 		-> loading page. use websocket to wait for server to respond
					-> home will send the id as a post param; hard code this number into the generated page
					-> on finish, go to "/done"
					-> on error, go to "/error"

"/done"				-> playlist URI is a form param, and will give link to the playlist

"/error"			-> page displays the error that is in the form param

TODO: if not authed when going to a page, redirect to spotify auth page and have it callback to the page they tried to go to

TODO: make playlist from user's top artists
TODO: make playlist from user's library (saves songs and albums)
TODO: on completion, give a button to go home and a button to go to the playlist
*/

func main() {
	ok := false
	state, ok = os.LookupEnv("SPOTIFY_STATE")
	if !ok {
		panic("SPOTIFY_STATE environment var not set")
	}
	authCache = cache.New(24*time.Hour, time.Hour)
	// first start an HTTP server
	// TODO: about page, error pages, favicon
	http.HandleFunc("/callback", completeAuth)
	http.HandleFunc("/index.html", homeHandler)
	http.HandleFunc("/do-the-roar", roarHandler)
	http.HandleFunc("/work-bitch", workHandler)
	http.HandleFunc("/Spotify.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "Spotify.png")
	})
	http.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "logo.png")
	})
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "favicon.ico")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/" {
			homeHandler(w, r)
			return
		}
		log.Println("Got request for:", r.URL.String())
	})
	http.ListenAndServe(":"+port, nil)
}

func newClient(token string) spotify.Client {
	return auth.NewClient(&oauth2.Token{
		AccessToken: token,
		TokenType:   "Bearer",
	})
}

func getBaseTemplateArgs() map[string]interface{} {
	args := make(map[string]interface{})
	data, err := ioutil.ReadFile("nav.html")
	if err != nil {
		log.Fatalln("Failed to load nav.html", err)
	}
	args["Nav"] = template.HTML(data)
	return args
}

func roarHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		log.Println("Failed to parse roar form:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlistID := r.PostForm.Get(playlistIDKKey)
	weeks := r.PostForm.Get(weeksKey)
	private := r.PostForm.Get(privateKey)
	roarTemplate := template.Must(template.ParseFiles("roar.html"))
	args := getBaseTemplateArgs()
	args["PlaylistID"] = playlistID
	args["Weeks"] = weeks
	args["Private"] = private
	err = roarTemplate.Execute(w, args)
	if err != nil {
		log.Println("Failed to execute roar template:", err)
	}
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	var numInputSongs, numArtists, numAlbums, numOutputSongs int

	// parse inputs
	err := r.ParseForm()
	if err != nil {
		log.Println("Failed to parse work form:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlistID := r.PostForm.Get("playlist")
	weeks, err := strconv.Atoi(r.PostForm.Get("weeks"))
	if err != nil || weeks < 1 || weeks > 4 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	private := false
	if r.PostForm.Get("private") == "true" {
		private = true
	}
	id := r.PostForm.Get("uuid")
	if playlistID == "" || id == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	token, ok := authCache.Get(id)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	client := newClient(token.(string))
	client.AutoRetry = true

	// Get input playlist
	playlist, err := client.GetPlaylist(spotify.ID(playlistID))
	if err != nil {
		log.Println("Error getting playlist:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Get all artists on playlist
	artists := make(map[spotify.ID]string)
	for {
		for _, track := range playlist.Tracks.Tracks {
			numInputSongs++
			for _, artist := range track.Track.Artists {
				artists[artist.ID] = artist.Name
			}
		}
		if err := client.NextPage(&playlist.Tracks); err != nil {
			if err != spotify.ErrNoMorePages {
				log.Println("Error getting next tracks from playlist", err)
			}
			break
		}
	}
	delete(artists, "") // Yeah so this happens
	numArtists = len(artists)

	// Make playlist
	user, err := client.CurrentUser()
	if err != nil {
		log.Println("Failed to get current user:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlist, err = client.CreatePlaylistForUser(user.ID, "PHRESH: "+playlist.Name, "The freshest tracks from the artists in "+playlist.Name, !private)
	if err != nil {
		log.Println("Failed make playlist:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	addedAlbums := make(map[spotify.ID]bool)

	// For each artist
	tracks := make([]spotify.ID, 0)
	// TODO: do each artist in new goroutine?
	// TODO: some sort of progress on client side?
	for artist, name := range artists {
		numArtists++
		// Get their albums
		market := spotify.MarketFromToken
		limit := 50
		albumPage, err := client.GetArtistAlbumsOpt(artist, &spotify.Options{
			Country: &market,
			Limit:   &limit,
		}, spotify.AlbumTypeAlbum, spotify.AlbumTypeSingle)
		if err != nil {
			log.Println("Failed to get artist albums:", err, name, artist)
			continue
		}
		for {
			for _, album := range albumPage.Albums {
				numAlbums++
				// If the album is new and we haven't already used it
				if album.ReleaseDateTime().After(time.Now().AddDate(0, 0, -7*weeks)) && !addedAlbums[album.ID] {
					addedAlbums[album.ID] = true
					// Add all tracks to new playlist
					trackPage, err := client.GetAlbumTracksOpt(album.ID, 50, -1)
					if err != nil {
						log.Println("Failed to get album tracks", err, album.Name, album.ID)
						continue
					}
					for {
						for _, track := range trackPage.Tracks {
							tracks = append(tracks, track.ID)
						}
						if err := client.NextPage(trackPage); err != nil {
							if err != spotify.ErrNoMorePages {
								log.Println("Error getting next tracks from album:", err, album.Name, album.ID)
							}
							break
						}
					}
					for len(tracks) >= 100 {
						_, err := client.AddTracksToPlaylist(playlist.ID, tracks[:100]...)
						if err != nil {
							log.Println("Failed to add a batch of songs:", err)
							break
						}
						numOutputSongs += 100
						tracks = tracks[100:]
					}
				}
			}
			if err := client.NextPage(albumPage); err != nil {
				if err != spotify.ErrNoMorePages {
					log.Println("Error getting next albums from artist", err, artist)
				}
				break
			}
		}
	}
	if len(tracks) > 0 {
		_, err := client.AddTracksToPlaylist(playlist.ID, tracks...)
		if err != nil {
			log.Println("Failed to add final batch of songs:", err)
		}
		numOutputSongs += len(tracks)
	}
	log.Printf("%v %v: # input songs: %v, # artists: %v, # albums parsed: %v, # tracks added: %v, dt: %v\n",
		user.DisplayName, playlist.Name, numInputSongs, numArtists, numAlbums, numOutputSongs, time.Now().Sub(startTime))
	// TODO: json response to include more info (report success with errors if we failed SOME things)
	io.WriteString(w, playlist.ExternalURLs["spotify"])
}
func homeHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieID)
	switch err {
	case http.ErrNoCookie:
		homeLoggedOutHandler(w, r)
	case nil:
		// check if user is logged in
		id := cookie.Value
		token, ok := authCache.Get(id)
		if !ok {
			homeLoggedOutHandler(w, r)
			break
		}
		homeLoggedInHandler(w, r, token.(string))
	default:
		log.Println("Unexpected error getting cookie:", err)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func homeLoggedInHandler(w http.ResponseWriter, r *http.Request, token string) {
	client := newClient(token)
	playlistPage, err := client.CurrentUsersPlaylists()
	if err != nil {
		log.Println("Failed getting playlists:", err)
		fmt.Fprintln(w, err)
		return
	}
	playlists := make([]struct {
		ID, Name string
	}, 0)
	for {
		for _, playlist := range playlistPage.Playlists {
			playlists = append(playlists, struct{ ID, Name string }{ID: string(playlist.ID), Name: playlist.Name})
			//fmt.Fprintf(w, "<a href=\"/do-the-roar?playlist_id=%v\">%v</a></br>", playlist.ID, playlist.Name)
		}
		err := client.NextPage(playlistPage)
		if err != nil {
			break
		}
	}
	sort.Slice(playlists, func(i, j int) bool {
		return playlists[i].Name < playlists[j].Name
	})
	loggedInTemplate := template.Must(template.ParseFiles("logged_in.html"))
	args := getBaseTemplateArgs()
	args["Playlists"] = playlists
	err = loggedInTemplate.Execute(w, args)
	if err != nil {
		log.Println("Failed to execute logged in template:", err)
	}
}

func homeLoggedOutHandler(w http.ResponseWriter, r *http.Request) {
	loggedOutTemplate := template.Must(template.ParseFiles("logged_out.html"))
	args := getBaseTemplateArgs()
	args["Auth"] = auth.AuthURL(state)
	err := loggedOutTemplate.Execute(w, args)
	if err != nil {
		log.Println("Failed to execute logged in template:", err)
	}
}

func completeAuth(w http.ResponseWriter, r *http.Request) {
	token, err := auth.Token(state, r)
	if err != nil {
		http.Error(w, "Couldn't get token", http.StatusForbidden)
		log.Fatal(err)
	}
	if st := r.FormValue("state"); st != state {
		http.NotFound(w, r)
		log.Fatalf("State mismatch: %s != %s\n", st, state)
	}
	id := uuid.New().String()
	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookieID,
		Value:   id,
		Expires: time.Time{},
	})
	dur := cache.DefaultExpiration
	if !token.Expiry.IsZero() {
		dur = token.Expiry.Sub(time.Now())
	}
	authCache.Add(id, token.AccessToken, dur)
	http.Redirect(w, r, "index.html", http.StatusSeeOther)
}
