package main

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/patrickmn/go-cache"
	"golang.org/x/oauth2"
	"log"
	"net/http"
	"os"
	"time"

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
	state     string
	authCache *cache.Cache
)

/*
"/", "/index.html"	-> home page
					-> if user is authenticated, it will allow them to pick a playlist and a duration
					-> use a form to do a post, with playlists in a dropdown box
					-> if user is not authenticated, there will be a login button

"/callback" 		-> oauth callback

"/do-the-roar" 		-> loading page. use websocket to wait for server to respond
					-> home will send the id as a post param; hard code this number into the generated page
					-> on finish, go to "/done"
					-> on error, go to "/error"

"/done"				-> playlist URI is a form param, and will give link to the playlist

"/error"			-> page displays the error that is in the form param

TODO: if not authed when going to a page, redirect to spotify auth page and have it callback to the page they tried to go to
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

func dateStringToTime(date, precision string) time.Time {
	format := ""
	switch precision {
	case "year":
		format = "2009"
	case "month":
		format = "2009-02"
	case "day":
		format = "2009-02-01"
	}
	t, err := time.Parse(format, date)
	fmt.Println(err)
	return t
}

func roarHandler(w http.ResponseWriter, r *http.Request) {
	// ensure auth
	cookie, err := r.Cookie(sessionCookieID)
	if err == http.ErrNoCookie {
		http.Redirect(w, r, "index.html", http.StatusSeeOther)
		return
	} else if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	id := cookie.Value
	token, ok := authCache.Get(id)
	if !ok {
		http.Redirect(w, r, "index.html", http.StatusSeeOther)
		return
	}
	// ensure playlistID
	err = r.ParseForm()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	playlistID := r.Form.Get(playlistIDKKey)
	client := newClient(token.(string))
	client.AutoRetry = true

	playlist, err := client.GetPlaylist(spotify.ID(playlistID))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Get all artists on playlist
	artists := make(map[spotify.ID]bool)
	for {
		for _, track := range playlist.Tracks.Tracks {
			for _, artist := range track.Track.Artists {
				artists[artist.ID] = true
			}
		}
		if err := client.NextPage(&playlist.Tracks);err != nil {
			break
		}
	}

	// Make playlist
	user, _ := client.CurrentUser()
	playlist, _ = client.CreatePlaylistForUser(user.ID, "FRESH: "+playlist.Name, "The freshest tracks from the artists in "+playlist.Name, true)

	// For each artist
	for artist := range artists {
		// Get their albums
		market := new(string)
		*market = spotify.MarketFromToken
		albumPage, _ := client.GetArtistAlbumsOpt(artist, &spotify.Options{
			Country: market,
		}, spotify.AlbumTypeAlbum, spotify.AlbumTypeSingle)
		for {
			for _, album := range albumPage.Albums {
				// If the album is new
				if album.ReleaseDateTime().After(time.Now().AddDate(0, -1, 0)) {
					// Add all tracks to new playlist
					trackPage, _ := client.GetAlbumTracks(album.ID)
					tracks := make([]spotify.ID, 0)
					for {
						for _, track := range trackPage.Tracks {
							tracks = append(tracks, track.ID)
						}
						if err := client.NextPage(trackPage);err != nil {
							break
						}
					}
					client.AddTracksToPlaylist(playlist.ID, tracks...)
				}
			}
			if err := client.NextPage(albumPage); err != nil {
				break
			}
		}
	}
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
	fmt.Fprintln(w, "<html><body>")
	defer fmt.Fprintln(w, "</body></html>")
	for {
		for _, playlist := range playlistPage.Playlists {
			fmt.Fprintf(w, "<a href=\"/do-the-roar?playlist_id=%v\">%v</a></br>", playlist.ID, playlist.Name)
		}
		err := client.NextPage(playlistPage)
		if err != nil {
			break
		}
	}
}

func homeLoggedOutHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "<html><body>")
	defer fmt.Fprintln(w, "</body></html>")
	fmt.Fprintf(w, `<a href="%v">Login</a>`, auth.AuthURL(state))
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
