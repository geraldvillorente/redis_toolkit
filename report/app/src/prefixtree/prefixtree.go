package prefixtree

import (
    "fmt"
    "strconv"
    "strings"
    "log"
    "github.com/mediocregopher/radix.v2/redis"
    "github.com/mediocregopher/radix.v2/pool"
    "errors"
    "sort"
    "sync"
    "time"
)

var wait sync.WaitGroup

type Node struct {
    // String representing the Redis key name.
    Key string

    // How many leaves does this node have. If this is a leaf node, the value will be 1.
    LeafCount int

    // The number of bytes this key's value is occupying in memory.
    Size int

    // The number of keys that actually have a size value. Useful for an accurate average key size.
    SizeCount int

    // The number of GET operations.
    Get int

    // The number of SET operations
    Set int

    // The time between 2 consecutive sets.
    Lifetime int

    // The number of keys that actually have a lifetime value. Useful for an accurate average life time.
    LifetimeCount int

    // The total amount of network traffic (in Bytes) that went in and out of this node.
    NetworkTraffic int

    // Pointer to Children nodes.
    Children []*Node
}

// Builds the a trie sub-structure recursively. 
func Buildsubtree(subtreeCh chan Node, keys []string, progressCh chan int, reportType *string, connectionPool *pool.Pool) {
    var parent_key string

    conn, err := connectionPool.Get()
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    node := Node{Key: "ROOT"}
    for j, key := range keys {
        prefixes := buildPrefixes(key)
        for i, prefix := range prefixes {
            if i == 0 {
                parent_key = "ROOT"
            } else {
                parent_key = prefixes[i-1]
            }
            node.addChild(prefix, parent_key)
        }
        // Show progress.
        progressCh <- j
    }
    // Populate the subtree.
    if *reportType == "memory" {
        node.PopulateForMemoryReport(conn)
    } else {
        node.PopulateForHitrateReport(conn)
    }

    subtreeCh <- node
    wait.Done()
}

// Builds the trie (or prefix tree) recursively after it decomposes the key by prefix.
func (node *Node) BuildTree(keys []string, reportType *string, connectionPool *pool.Pool) {
    
    branchIndexes := getBranchIndexes(keys)
    channelCount := len(branchIndexes)
    wait.Add(channelCount)

    subtreeCh  := make(chan Node, channelCount)
    progressCh := make(chan int, channelCount)

    start := time.Now()

    startIndex := 0
    for i:=0; i<channelCount; i++ {
        endIndex := branchIndexes[i]
        go Buildsubtree(subtreeCh, keys[startIndex:endIndex], progressCh, reportType, connectionPool)
        startIndex = endIndex
    }

    go ShowProgressBar(progressCh, float64(len(keys)))

    wait.Wait()

    for i:=0; i<channelCount; i++ {
        n := <-subtreeCh
        node.Children = append(node.Children, n.Children...)
    }

    // Fix root data.
    for _, child := range node.Children {
        if *reportType == "memory" {
            node.Size      += child.Size
            node.LeafCount += child.LeafCount
        } else {
            node.Get += child.Get
            node.Set += child.Set
            node.Size += child.Size
            node.SizeCount += child.SizeCount
            node.LeafCount += child.LeafCount
            node.Lifetime += child.Lifetime
            node.LifetimeCount  += child.LifetimeCount
            node.NetworkTraffic += child.NetworkTraffic
        }
    }
    fmt.Println("Built tree in: ", time.Since(start))
}

// Builds the trie (or prefix tree) recursively after it decomposes the key by prefix.
func (node *Node) BuildTreeDeprecated(keys []string) {
    var parent_key string
    start := time.Now()
    totalKeys := len(keys)

    for i, key := range keys {
        prefixes := buildPrefixes(key)
        for i, prefix := range prefixes {
            if i == 0 {
                parent_key = "ROOT"
            } else {
                parent_key = prefixes[i-1]
            }
            node.addChild(prefix, parent_key)
        }
        ProgressBar(float64(i + 1), float64(totalKeys))
    }

    fmt.Println("Built tree in: ", time.Since(start))
}

// Adds child nodes to the trie.
func (node *Node) addChild(key string, parent_key string) {
    child_keys := node.getChildrenKeys()

    // If I reached the parent.
    if parent_key == node.Key {
        if !contains(child_keys, key) {
            node.Children = append(node.Children, &Node{Key: key})
        }
    } else {
        if len(node.Children) > 0 {
            for _, child := range node.Children {
                if strings.HasPrefix(parent_key, child.Key) {
                    child.addChild(key, parent_key)
                }
            }
        }
    }
}

// Returns a list of children keys.
func (node *Node) getChildrenKeys() []string {
    var keys []string

    if len(node.Children) > 0 {
        for _, child := range node.Children {
            keys = append(keys, child.Key)
        }
    }
    
    return keys
}

// Checks if node is leaf.
func (node *Node) IsLeaf() bool {
    if (len(node.Children) == 0) {
        return true
    }
    return false
}

func (node *Node) DumpTree(level int) {
    var info string

    if node.IsLeaf() {
        info = fmt.Sprintf(" - (leaf, size: %d)", node.Size)
    } else {
        info = fmt.Sprintf(" - (leaves: %d, size: %d)", node.LeafCount, node.Size)
    }

    fmt.Println(node.Key, info)

    if (len(node.Children) > 0) {
        for _, child := range node.Children {
            fmt.Printf(strings.Repeat("----", level))
            child.DumpTree(level+1)
        }
    }
}


// Executes one last pass through the finished tree. Populating the tree for running a memory distribution report.
func (node *Node) PopulateForMemoryReport(conn *redis.Client) map[string]int {
    for _, child := range node.Children {
        if child.IsLeaf() {
            // Query Redis for the key information (gets, set, hitrate, etc).
            size := redisKeySize(conn, child.Key)

            child.Size      = size
            child.LeafCount = 1

            // Increment leaf count, size.
            node.LeafCount  += 1
            node.Size       += child.Size
        } else {
            r := child.PopulateForMemoryReport(conn)

            node.LeafCount += r["LeafCount"]
            node.Size      += r["Size"]
        }
    }
    
    return map[string]int{
        "LeafCount": node.LeafCount,
        "Size": node.Size,
    }
}

// Executes one last pass through the finished tree. Populating the tree for running a hit rate report.
func (node *Node) PopulateForHitrateReport(conn *redis.Client) map[string]int {
    for _, child := range node.Children {
        if child.IsLeaf() {
            // Query Redis for the key information (gets, set, hitrate, etc).
            data, err := conn.Cmd("HGETALL", child.Key).Map()
            if err != nil {
                log.Print(err)
            }

            child.LeafCount = 1

            child.Get, _ = strconv.Atoi(data["get"])
            child.Set, _ = strconv.Atoi(data["set"])

            sizeBytes, _ := strconv.ParseFloat(data["size"], 64)
            child.Size = int(sizeBytes)

            lifetime, _ := strconv.ParseFloat(data["lifetime"], 64)
            child.Lifetime = int(lifetime)

            // For average calculation.
            if child.Size > 0 {
                child.SizeCount = 1
                node.SizeCount += 1

                child.NetworkTraffic = (child.Get + child.Set) * child.Size
            }

            if child.Lifetime > 0 {
                child.LifetimeCount = 1
                node.LifetimeCount += 1
            }
            
            // Increment parent leaf count, get, set, size.
            node.LeafCount += 1

            // Increment the number of gets and sets.
            node.Get       += child.Get
            node.Set       += child.Set

            // Increment the total size and size_count.
            node.Size      += child.Size

            // Increment parent node lifetime.
            node.Lifetime += child.Lifetime

            node.NetworkTraffic += child.NetworkTraffic

        } else {
            r := child.PopulateForHitrateReport(conn)

            node.LeafCount += r["LeafCount"]
            node.Get       += r["Get"]
            node.Set       += r["Set"]

            node.Size      += r["Size"]
            node.SizeCount += r["SizeCount"]

            node.Lifetime       += r["Lifetime"]
            node.LifetimeCount  += r["LifetimeCount"]

            node.NetworkTraffic += r["NetworkTraffic"]
        }
    }
    
    return map[string]int{
        "LeafCount": node.LeafCount,
        "Get": node.Get,
        "Set": node.Set,
        "Size": node.Size,
        "SizeCount": node.SizeCount,
        "Lifetime": node.Lifetime,
        "LifetimeCount": node.LifetimeCount,
        "NetworkTraffic": node.NetworkTraffic,
    }
}

// Returns the key size of a specific key. Depending on the key's type, different commands need to be executed.
func redisKeySize(conn *redis.Client, key string) int {
    var object_size int
    var data []string
    
    keyType, err := conn.Cmd("TYPE", key).Str()
    
    if err != nil {
        log.Print("CMD: TYPE, Key: ", key)
        log.Print(err)
    }

    switch keyType {
        case "hash":
            data, err = conn.Cmd("HGETALL", key).List()
            if err != nil {
                log.Print("CMD: HGETALL, Key: ", key)
                log.Print(err)
            }
            for _, k := range data {
                object_size += len(k)
            }
            
        case "set":
            data, err = conn.Cmd("SMEMBERS", key).List()
            if err != nil {
                log.Print("CMD: SMEMBERS, Key: ", key)
                log.Print(err)
            }

            for _, k := range data {
                object_size += len(k)
            }
            
        case "string":
            data, err := conn.Cmd("GET", key).Str()
            if err != nil {
                log.Print("CMD: GET, Key: ", key)
                log.Print(err)
            }

            object_size = len(data)
    }

    return object_size
}

func getNextBranchIndex(keys []string, startFromIndex int, level int) (int, error) {
    candidateIndex, lastIndex := startFromIndex, len(keys) - 1
    if candidateIndex > lastIndex {
        return candidateIndex, nil
    }

    for i:=0; i<lastIndex; i++ {
        if candidateIndex >= lastIndex {
            return candidateIndex + 1, nil
        }
        currentKeyParts := strings.Split(keys[candidateIndex], ":")
        nextKeyParts    := strings.Split(keys[candidateIndex + 1], ":")

        candidateIndex++
        if currentKeyParts[level] != nextKeyParts[level] {
            // Return the index for the next different key for better splitting.
            return candidateIndex, nil
        }
    }
    return -1, errors.New("Reached end of keys without finding a next candidate")
}

func getBranchIndexes(keys []string) []int {
    var branchIndexes []int

    sort.Sort(sort.StringSlice(keys))

    level := 0
    startIndex := 0
    lastIndex  := len(keys) - 1

    for {
        endIndex, err := getNextBranchIndex(keys, startIndex, level)
        if err != nil {
            level++
            continue
        }

        branchIndexes = append(branchIndexes, endIndex)
        startIndex = endIndex
        // Check if we reached the end of the list
        if endIndex > lastIndex {
            //return branchIndexes
            break
        }
    }
    return branchIndexes
}

// Builds a list of prefixes, each of them one level deeper than its predecesor. 
// Example:
//           - pantheon-redis
//           - pantheon-redis:cache_page
//           - pantheon-redis:cache_page:www.example.com
func buildPrefixes(key string) []string {
    var prefixes []string
    key_parts := strings.Split(key, ":")
    for i, key := range key_parts {
        if i == 0 {
            prefixes = append(prefixes, key)
        } else {
            prefixes = append(prefixes, prefixes[i-1] + ":" + key)            
        }
    }
    return prefixes
}

func contains(slice []string, item string) bool {
    set := make(map[string]struct{}, len(slice))
    for _, s := range slice {
        set[s] = struct{}{}
    }
    _, ok := set[item] 
    return ok
}

func ShowProgressBar(progressCh chan int, total float64 /*suffix ...string*/) {
    var count float64
    count = 0.0

    for {
        select {
            case <- progressCh:
                count++
                if count <= total {
                    ProgressBar(count, total)
                } else {
                    ProgressBar(total, total, "Populating the report. Please wait ...")
                }
            default:
                break
        }
    }
}

// Shows visual progress in the form of a loading bar.
func ProgressBar(count float64, total float64, suffix ...string) {
    barLen := 60
    filledLen := float64(barLen) * count / total

    percents := int((100 * count) / total)
    bar := strings.Repeat("#", int(filledLen)) + strings.Repeat("_", (int(barLen) - int(filledLen)))

    if suffix == nil {
        fmt.Printf("Building report tree [%s] %d%%\r", bar, percents)    
    } else {
        fmt.Printf("Building report tree [%s] %d%% %s\r", bar, percents, suffix)
    }
}