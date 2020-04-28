package main

import (
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/patrickmn/go-cache"
	"golang.org/x/oauth2"

	"github.com/zmb3/spotify"
)

const (
	redirectURI     = "http://localhost:8080/callback"
	sessionCookieID = "session_id"
	playlistIDKKey  = "playlist_id"
)

var (
	auth = spotify.NewAuthenticator(redirectURI,
		spotify.ScopePlaylistReadCollaborative,
		spotify.ScopePlaylistReadPrivate,
		spotify.ScopePlaylistModifyPrivate,
		spotify.ScopePlaylistModifyPublic)
	roarTemplate      = template.Must(template.ParseFiles("roar.html"))
	loggedInTemplate  = template.Must(template.ParseFiles("logged_in.html"))
	loggedOutTemplate = template.Must(template.ParseFiles("logged_out.html"))
	state             string
	authCache         *cache.Cache
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
	// TODO: for heroku, get port from os.LookupEnv("PORT")
	if !ok {
		panic("SPOTIFY_STATE environment var not set")
	}
	authCache = cache.New(24*time.Hour, time.Hour)
	// first start an HTTP server
	http.HandleFunc("/callback", completeAuth)
	http.HandleFunc("/index.html", homeHandler)
	http.HandleFunc("/do-the-roar", roarHandler)
	http.HandleFunc("/work-bitch", workHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/" {
			homeHandler(w, r)
			return
		}
		log.Println("Got request for:", r.URL.String())
	})
	http.ListenAndServe(":8080", nil)
}

func newClient(token string) spotify.Client {
	return auth.NewClient(&oauth2.Token{
		AccessToken: token,
		TokenType:   "Bearer",
	})
}

func roarHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlistID := r.PostForm.Get(playlistIDKKey)
	weeks := r.PostForm.Get("weeks")
	private := r.PostForm.Get("private")
	roarTemplate.Execute(w, struct {
		PlaylistID, Weeks, Private string
	}{
		PlaylistID: playlistID,
		Weeks:      weeks,
		Private:    private,
	})
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	// ensure playlistID
	err := r.ParseForm()
	if err != nil {
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

	playlist, err := client.GetPlaylist(spotify.ID(playlistID))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Get all artists on playlist
	artists := make(map[spotify.ID]string)
	for {
		for _, track := range playlist.Tracks.Tracks {
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

	// Make playlist
	user, err := client.CurrentUser()
	if err != nil {
		log.Println("Failed to get current user:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlist, err = client.CreatePlaylistForUser(user.ID, "FRESH: "+playlist.Name, "The freshest tracks from the artists in "+playlist.Name, !private)
	if err != nil {
		log.Println("Failed make playlist:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// For each artist
	tracks := make([]spotify.ID, 0)
	for artist, name := range artists {
		// Get their albums
		market := new(string)
		*market = spotify.MarketFromToken
		albumPage, err := client.GetArtistAlbumsOpt(artist, &spotify.Options{
			Country: market,
		}, spotify.AlbumTypeAlbum, spotify.AlbumTypeSingle)
		if err != nil {
			log.Println("Failed to get artist albums:", err, name, artist)
			continue
		}
		for {
			for _, album := range albumPage.Albums {
				// If the album is new
				if album.ReleaseDateTime().After(time.Now().AddDate(0, 0, -7*weeks)) {
					// Add all tracks to new playlist
					trackPage, err := client.GetAlbumTracks(album.ID)
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
	}
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
		w.WriteHeader(http.StatusBadRequest)
	}
}

func homeLoggedInHandler(w http.ResponseWriter, r *http.Request, token string) {
	client := newClient(token)
	playlistPage, err := client.CurrentUsersPlaylists()
	if err != nil {
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
	loggedInTemplate.Execute(w, playlists)
}

func homeLoggedOutHandler(w http.ResponseWriter, r *http.Request) {
	loggedOutTemplate.Execute(w, auth.AuthURL(state))
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
	authCache.Add(id, token.AccessToken, cache.DefaultExpiration)
	http.Redirect(w, r, "index.html", http.StatusSeeOther)
}

// TODO: home page with a login button
// home page will be te main page if the 'session_id' cookie is valid
