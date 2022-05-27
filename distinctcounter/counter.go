package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"git.torproject.org/pluggable-transports/snowflake.git/v2/common/ipsetsink/sinkcluster"
)

func main() {
	inputFile := flag.String("in", "", "")
	start := flag.String("start", "", "")
	end := flag.String("end", "", "")
	flag.Parse()
	startTime, err := time.Parse(time.UnixDate, *start)
	if err != nil {
		log.Fatal("unable to parse start time:", err)
	}
	endTime, err := time.Parse(time.UnixDate, *end)
	if err != nil {
		log.Fatal("unable to parse end time:", err)
	}
	fd, err := os.Open(*inputFile)
	if err != nil {
		log.Fatal("unable to open input file:", err)
	}
	counter := sinkcluster.NewClusterCounter(startTime, endTime)
	result, err := counter.Count(fd)
	if err != nil {
		log.Fatal("unable to count:", err)
	}
	fmt.Printf("sum = %v\n", result.Sum)
	fmt.Printf("chunkIncluded = %v\n", result.ChunkIncluded)
}
