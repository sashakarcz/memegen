package main

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "html/template"
    "io"
    "log"
    "net/http"

    "github.com/gofiber/fiber/v2"
    "github.com/redis/go-redis/v9"

    _ "github.com/mattn/go-sqlite3"
)

// Meme struct (metadata only)
type Meme struct {
    ID         int
    Template   string
    TopText    string
    BottomText string
    URL        string
    Votes      int
}

// Memegen Template struct
type MemeTemplate struct {
    ID       string `json:"id"`
    Name     string `json:"name"`
    BlankURL string `json:"blank"`
    Example  struct {
        URL string `json:"url"`
    } `json:"example"`
}

var (
    db         *sql.DB
    redisClient *redis.Client
    memegenAPI  = "http://localhost:5002"
    ctx        = context.Background()
)

func main() {
    var err error
    db, err = sql.Open("sqlite3", "./database.db")
    if err != nil {
        log.Fatal(err)
    }
    createTable()

    // Initialize Redis for caching images
    redisClient = redis.NewClient(&redis.Options{
        Addr:     "localhost:6379",
        Password: "",
        DB:       0,
    })

    app := fiber.New()
    app.Static("/static", "./static")

    // Route: Homepage (shows all memes)
    app.Get("/", func(c *fiber.Ctx) error {
        memes := getAllMemes()
        return renderTemplate(c, "index.html", memes)
    })

    // Route: Meme Creation Form
    app.Get("/generate", func(c *fiber.Ctx) error {
        templates, _ := fetchMemegenTemplates()
        return renderTemplate(c, "meme_form.html", templates)
    })

    // Route: Generate Meme
    app.Post("/generate", func(c *fiber.Ctx) error {
        templateName := c.FormValue("template")
        topText := c.FormValue("top")
        bottomText := c.FormValue("bottom")
        url := fmt.Sprintf("%s/images/%s/%s/%s.png", memegenAPI, templateName, topText, bottomText)
        saveMeme(templateName, topText, bottomText, url)
        return c.Redirect("/")
    })

    // Route: Upvote Meme
    app.Post("/vote/:id/up", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        _, err := db.Exec("UPDATE memes SET votes = votes + 1 WHERE id = ?", memeID)
        if err != nil {
            return err
        }
        return c.Redirect("/")
    })

    // Route: Downvote Meme
    app.Post("/vote/:id/down", func(c *fiber.Ctx) error {
        memeID := c.Params("id")
        _, err := db.Exec("UPDATE memes SET votes = votes - 1 WHERE id = ?", memeID)
        if err != nil {
            return err
        }
        return c.Redirect("/")
    })
    
    // Proxy Memegen API through Fiber
    app.Get("/api/images/:template/:top/:bottom.png", func(c *fiber.Ctx) error {
    template := c.Params("template")
    top := c.Params("top")
    bottom := c.Params("bottom")

    // Construct the correct Memegen API URL
    memegenURL := fmt.Sprintf("%s/images/%s/%s/%s.png", memegenAPI, template, top, bottom)
    log.Println("Proxying request to:", memegenURL)

    // Fetch the image from Memegen API
    resp, err := http.Get(memegenURL)
    if err != nil {
        log.Println("Error fetching image:", err)
        return c.Status(500).SendString("Error fetching meme image")
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        log.Println("Memegen API returned 404:", memegenURL)
        return c.Status(404).SendString("Meme not found")
    }

    // Read the image into memory
    imageBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        log.Println("Error reading meme image:", err)
        return c.Status(500).SendString("Error processing meme image")
    }

    // Serve the image with correct Content-Type
    c.Set("Content-Type", "image/png")
    return c.Send(imageBytes)
    })
 

    // Route: Serve Meme via Redis
    app.Get("/meme/:id", serveMeme)

    log.Fatal(app.Listen(":8181"))
}

// Create DB table
func createTable() {
    query := `CREATE TABLE IF NOT EXISTS memes (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        template TEXT,
        topText TEXT,
        bottomText TEXT,
        url TEXT,
        votes INTEGER DEFAULT 0
    )`
    _, err := db.Exec(query)
    if err != nil {
        log.Fatal(err)
    }
}

// Fetch all memes
func getAllMemes() []Meme {
    rows, err := db.Query("SELECT id, template, topText, bottomText, url, votes FROM memes ORDER BY votes DESC")
    if err != nil {
        log.Println("Error fetching memes:", err)
        return nil
    }
    defer rows.Close()

    var memes []Meme
    for rows.Next() {
        var meme Meme
        err := rows.Scan(&meme.ID, &meme.Template, &meme.TopText, &meme.BottomText, &meme.URL, &meme.Votes)
        if err != nil {
            log.Println("Error scanning meme:", err)
            continue
        }
        memes = append(memes, meme)
    }
    return memes
}

// Fetch Memegen templates
func fetchMemegenTemplates() ([]MemeTemplate, error) {
    // Try to get templates from Redis
    templates, err := getTemplatesFromRedis()
    if err != nil {
        // Fallback to Memegen API
        return fetchTemplatesFromAPI()
    }
    return templates, nil
}

func getTemplatesFromRedis() ([]MemeTemplate, error) {
    ctx := context.Background()
    templatesJSON, err := redisClient.Get(ctx, "memegen-templates").Result()
    if err != nil {
        return nil, err
    }

    var templates []MemeTemplate
    err = json.Unmarshal([]byte(templatesJSON), &templates)
    if err != nil {
        return nil, err
    }
    return templates, nil
}

func fetchTemplatesFromAPI() ([]MemeTemplate, error) {
    // Existing code to fetch templates from Memegen API
    resp, err := http.Get(memegenAPI + "/templates")
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    var templates []MemeTemplate
    json.Unmarshal(body, &templates)

    // Store templates in Redis for next time
    storeTemplatesInRedis(templates)
    return templates, nil
}

func storeTemplatesInRedis(templates []MemeTemplate) {
    ctx := context.Background()
    templatesJSON, err := json.Marshal(templates)
    if err != nil {
        log.Println("Error marshaling templates:", err)
        return
    }

    err = redisClient.Set(ctx, "memegen-templates", templatesJSON, 0).Err()
    if err != nil {
        log.Println("Error storing templates in Redis:", err)
    }
}

// Save meme metadata
func saveMeme(template, topText, bottomText, url string) {
    _, err := db.Exec("INSERT INTO memes (template, topText, bottomText, url, votes) VALUES (?, ?, ?, ?, 0)", template, topText, bottomText, url)
    if err != nil {
        log.Println("Error inserting meme:", err)
    }
}

// Render HTML template
func renderTemplate(c *fiber.Ctx, templateName string, data interface{}) error {
    tmpl, err := template.ParseFiles("templates/" + templateName)
    if err != nil {
        log.Println("Error parsing template:", err)
        return c.Status(500).SendString("Template rendering error")
    }
    c.Set("Content-Type", "text/html; charset=utf-8")
    return tmpl.Execute(c.Response().BodyWriter(), data)
}

// Generate meme on demand
func generateMeme(template, topText, bottomText string) ([]byte, error) {
    url := fmt.Sprintf("%s/images/%s/%s/%s.png", memegenAPI, template, topText, bottomText)
    resp, err := http.Get(url)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    return io.ReadAll(resp.Body)
}

// Serve meme via Redis
func serveMeme(c *fiber.Ctx) error {
    id := c.Params("id")
    row := db.QueryRow("SELECT url FROM memes WHERE id = ?", id)
    var memeURL string
    err := row.Scan(&memeURL)
    if err != nil {
        return err
    }

    // Check if meme is cached in Redis
    img, err := redisClient.Get(ctx, memeURL).Bytes()
    if err != nil {
        // Generate meme if not cached
        template, topText, bottomText := getMemeParams(memeURL)
        img, err = generateMeme(template, topText, bottomText)
        if err != nil {
            return err
        }

        // Cache meme in Redis
        err = redisClient.Set(ctx, memeURL, img, 0).Err()
        if err != nil {
            log.Println("Error caching meme:", err)
        }
    }

    // Serve image directly from Go application
    c.Set("Content-Type", "image/png")
    return c.Send(img)
}

// Get meme parameters from SQLite DB
func getMemeParams(url string) (template, topText, bottomText string) {
    row := db.QueryRow("SELECT template, topText, bottomText FROM memes WHERE url = ?", url)
    err := row.Scan(&template, &topText, &bottomText)
    if err != nil {
        log.Println("Error retrieving meme params:", err)
    }
    return
}
