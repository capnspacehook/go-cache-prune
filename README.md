# go-cache-prune

A utility to prune unneeded files from Go's module and build caches. The motivation was using [`actions/cache`](https://github.com/actions/cache) to [update existing Github Actions caches](https://github.com/actions/cache/blob/main/tips-and-workarounds.md#update-a-cache) with only necessary files. `go-cache-prune` will listen for file access or create events in the Go caches, and when it is sent a NOHUP signal it will delete any unused files in both Go caches.

Signaling a running `go-cache-prune` process can easily be done with `go-cache-prune -signal`.
