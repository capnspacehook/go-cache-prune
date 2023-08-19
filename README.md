# go-cache-prune

A utility to prune unneeded files from Go's module and build caches. The motivation was using [`actions/cache`](https://github.com/actions/cache) to [update existing Github Actions caches](https://github.com/actions/cache/blob/main/tips-and-workarounds.md#update-a-cache) with only necessary files to reduce their size. `go-cache-prune` will listen for file access or create events for files in the Go caches, and keep track of what files were used. When `go-cache-prune` receives a SIGHUP signal, it will stop listening for file events and delete all files in both Go caches it didn't record as being used.

Signaling a running `go-cache-prune` process can easily be done with `go-cache-prune -signal`.
