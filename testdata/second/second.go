package main

import "encoding/json"

func main() {
	i := 420
	m, err := json.Marshal(&i)
	println(m, err)
}
