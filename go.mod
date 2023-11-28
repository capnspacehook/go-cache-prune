module github.com/capnspacehook/go-cache-prune

go 1.21

require (
	github.com/fsnotify/fsnotify v1.6.0
	github.com/sethvargo/go-githubactions v1.1.0
	golang.org/x/mod v0.12.0
	golang.org/x/sys v0.15.0
)

require github.com/sethvargo/go-envconfig v0.8.0 // indirect

// support setting inotify masks directly
replace github.com/fsnotify/fsnotify => github.com/capnspacehook/fsnotify v0.0.0-20230821220533-21b7af8893a0
