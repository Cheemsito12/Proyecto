package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Configuración
const (
	// Port = ":8080" // YA NO SE USA FIJO, se detecta en main()
	TokenFile  = "token.txt"
	ApiBaseURL = "https://api.decolecta.com/v1/reniec/dni?numero="

	// ⚡ VELOCIDAD AUMENTADA (3 workers)
	MaxWorkers   = 3                      // 3 Consultas simultáneas
	RequestDelay = 200 * time.Millisecond // Pausa reducida a 0.2s
	MaxRetries   = 3                      // Reintentos en caso de error

	ReadTimeout  = 0 // Desactivamos timeout global de lectura para permitir streaming largo
	WriteTimeout = 0 // Desactivamos timeout de escritura para streaming
)

// Estructuras de Datos
type APIResponse struct {
	FirstName      string `json:"first_name"`
	FirstLastName  string `json:"first_last_name"`
	SecondLastName string `json:"second_last_name"`
	Message        string `json:"message"`
}

type ComparisonRow struct {
	ID           int
	DNI          string
	NombreInput  string
	PaternoInput string
	MaternoInput string
	// API Data
	NombreAPI  string
	PaternoAPI string
	MaternoAPI string
	// Logic
	MatchNombre  bool
	MatchPaterno bool
	MatchMaterno bool
	HasError     bool
	ErrorMessage string
	IsPending    bool // Para la vista inicial
}

var templates *template.Template

func init() {
	var err error
	// Funciones auxiliares para los templates
	funcMap := template.FuncMap{
		"safe": func(s string) template.HTML {
			return template.HTML(s)
		},
	}
	templates, err = template.New("").Funcs(funcMap).ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("Error cargando templates: %v", err)
	}
}

func main() {
	http.HandleFunc("/", authMiddleware(handleIndex))
	http.HandleFunc("/guardar-token", handleSaveToken)
	http.HandleFunc("/consultar", authMiddleware(handleConsultar))

	// --- CAMBIO PARA RENDER Y NUBE ---
	// Render te asigna un puerto en la variable de entorno PORT.
	// Si no existe (estás en local), usamos el 8080.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Configuración de servidor sin timeouts estrictos para soportar SSE/Streaming
	server := &http.Server{
		Addr:    ":" + port, // Usamos el puerto dinámico
		Handler: nil,
	}

	fmt.Printf("🚀 Servidor corriendo en puerto :%s\n", port)
	fmt.Printf("⚡ MODO STREAMING ACTIVADO: %d hilos. Actualización en tiempo real.\n", MaxWorkers)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// --- Middleware & Helpers ---

func getToken() string {
	content, err := os.ReadFile(TokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := getToken()
		if token == "" {
			if err := templates.ExecuteTemplate(w, "formulario_token.html", nil); err != nil {
				http.Error(w, "Error interno de template", http.StatusInternalServerError)
			}
			return
		}
		next(w, r)
	}
}

// --- Handlers ---

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	templates.ExecuteTemplate(w, "formulario_consulta.html", nil)
}

func handleSaveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	token := r.FormValue("token")
	if token == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	os.WriteFile(TokenFile, []byte(token), 0644)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleConsultar usa Streaming HTML para actualizaciones en tiempo real
func handleConsultar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	// 1. Setup de Streaming
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming no soportado", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Accel-Buffering", "no") // Deshabilitar buffering en Nginx/Proxies si los hubiera

	// 2. Procesar Inputs
	rawDnis := r.FormValue("dnis")
	rawNombres := r.FormValue("nombres")
	token := getToken()

	scannerDNI := bufio.NewScanner(strings.NewReader(rawDnis))
	scannerNames := bufio.NewScanner(strings.NewReader(rawNombres))

	var initialRows []ComparisonRow

	// Preparamos las filas iniciales (Estado "Pendiente")
	idCounter := 0
	for scannerDNI.Scan() {
		dni := strings.TrimSpace(scannerDNI.Text())
		if dni == "" {
			continue
		}

		lineaNombre := ""
		if scannerNames.Scan() {
			lineaNombre = scannerNames.Text()
		}

		parts := strings.Split(lineaNombre, "\t")
		nombreIn, patIn, matIn := "", "", ""
		if len(parts) >= 1 {
			nombreIn = strings.TrimSpace(parts[0])
		}
		if len(parts) >= 2 {
			patIn = strings.TrimSpace(parts[1])
		}
		if len(parts) >= 3 {
			matIn = strings.TrimSpace(parts[2])
		}

		initialRows = append(initialRows, ComparisonRow{
			ID:           idCounter,
			DNI:          dni,
			NombreInput:  nombreIn,
			PaternoInput: patIn,
			MaternoInput: matIn,
			IsPending:    true, // Bandera para mostrar spinner
		})
		idCounter++
	}

	// 3. Renderizar la página inicial (Tabla con spinners)
	data := map[string]interface{}{
		"Resultados": initialRows,
		"Total":      len(initialRows),
	}
	if err := templates.ExecuteTemplate(w, "tabla_resultados.html", data); err != nil {
		fmt.Printf("Error template: %v\n", err)
		return
	}
	flusher.Flush() // Enviar al navegador inmediatamente

	// 4. Iniciar Procesamiento en Background
	var wg sync.WaitGroup
	resultsChan := make(chan ComparisonRow)
	sem := make(chan struct{}, MaxWorkers)
	client := &http.Client{Timeout: 30 * time.Second}

	// Productor (Workers)
	go func() {
		for _, row := range initialRows {
			wg.Add(1)

			// Pausa ligera entre lanzamientos
			time.Sleep(RequestDelay)

			go func(r ComparisonRow) {
				defer wg.Done()
				sem <- struct{}{} // Adquirir semáforo
				defer func() { <-sem }()

				// --- Retry Logic ---
				var resp *http.Response
				var err error
				success := false

				for attempt := 0; attempt < MaxRetries; attempt++ {
					req, _ := http.NewRequest("GET", ApiBaseURL+r.DNI, nil)
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("User-Agent", "Go-Validator/3.0")

					resp, err = client.Do(req)

					if err == nil {
						if resp.StatusCode == 429 {
							resp.Body.Close()
							// Reintento específico para 429
							time.Sleep(time.Duration(2+attempt) * time.Second)
							continue
						}
						success = true
						break
					} else {
						time.Sleep(1 * time.Second)
					}
				}

				r.IsPending = false // Ya no está pendiente

				if !success || err != nil {
					r.HasError = true
					r.ErrorMessage = "Error Red"
					if err == nil && resp != nil {
						r.ErrorMessage = fmt.Sprintf("HTTP %d", resp.StatusCode)
					}
				} else {
					defer resp.Body.Close()
					bodyBytes, _ := io.ReadAll(resp.Body)

					if resp.StatusCode == 200 {
						var apiData APIResponse
						if json.Unmarshal(bodyBytes, &apiData) == nil {
							r.NombreAPI = apiData.FirstName
							r.PaternoAPI = apiData.FirstLastName
							r.MaternoAPI = apiData.SecondLastName

							r.MatchNombre = strings.EqualFold(r.NombreInput, r.NombreAPI)
							r.MatchPaterno = strings.EqualFold(r.PaternoInput, r.PaternoAPI)
							r.MatchMaterno = strings.EqualFold(r.MaternoInput, r.MaternoAPI)
						} else {
							r.HasError = true
							r.ErrorMessage = "JSON Error"
						}
					} else {
						r.HasError = true
						r.ErrorMessage = fmt.Sprintf("HTTP %d", resp.StatusCode)
					}
				}
				resultsChan <- r
			}(row)
		}
		wg.Wait()
		close(resultsChan)
	}()

	// 5. Consumidor (Streaming de Scripts)
	// Recibimos resultados conforme llegan y enviamos <script> para actualizar el DOM
	for res := range resultsChan {
		htmlContent := generateRowHTML(res)
		// Enviamos un pequeño script que busca el ID de la fila y reemplaza su contenido
		// y actualiza las clases CSS según el resultado
		fmt.Fprintf(w, "<script>updateRow(%d, `%s`, %t);</script>\n", res.ID, htmlContent, res.HasError)
		flusher.Flush()
	}
}

// generateRowHTML crea el HTML interno de la fila (TDs)
func generateRowHTML(r ComparisonRow) string {
	// Clases CSS
	matchClass := "match-ok"
	failClass := "match-fail"

	// Helper para clases condicionales
	getClass := func(match bool) string {
		if match {
			return matchClass
		}
		return failClass
	}

	// Construimos el HTML manualmente para inyectarlo vía JS
	// Usamos backticks en JS, así que cuidado con escaparlos si fuera necesario,
	// pero aquí es HTML simple.

	errorBadge := ""
	if r.HasError {
		errorBadge = fmt.Sprintf(`<span class="block text-[10px] text-red-500 font-bold">%s</span>`, r.ErrorMessage)
	}

	// Botón copiar individual
	copyBtn := fmt.Sprintf(`
        <button onclick="copyRow(%d)" class="ml-2 text-slate-400 hover:text-blue-600 transition-colors p-1 rounded-full hover:bg-blue-50" title="Copiar datos RENIEC">
            <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" />
            </svg>
        </button>
    `, r.ID)

	if r.HasError {
		copyBtn = "" // No mostrar botón copiar si hay error
	}

	html := fmt.Sprintf(`
        <td class="px-4 py-4 whitespace-nowrap text-sm font-mono text-slate-900 border-r border-slate-100">
            %s %s
        </td>
        
        <!-- Input Data -->
        <td class="px-4 py-3 whitespace-nowrap text-sm text-center border-l %s">%s</td>
        <td class="px-4 py-3 whitespace-nowrap text-sm text-center %s">%s</td>
        <td class="px-4 py-3 whitespace-nowrap text-sm text-center border-r border-slate-200 %s">%s</td>

        <!-- API Data (Con IDs para copiar) -->
        <td class="px-4 py-3 whitespace-nowrap text-sm text-slate-600 text-center border-l border-slate-100 font-medium">
            <span id="nom-%d">%s</span>
        </td>
        <td class="px-4 py-3 whitespace-nowrap text-sm text-slate-600 text-center font-medium">
            <span id="pat-%d">%s</span>
        </td>
        <td class="px-4 py-3 whitespace-nowrap text-sm text-slate-600 text-center border-r border-slate-100 font-medium flex items-center justify-center gap-2">
            <span id="mat-%d">%s</span>
            %s
        </td>
    `,
		r.DNI, errorBadge,
		getClass(r.MatchNombre), r.NombreInput,
		getClass(r.MatchPaterno), r.PaternoInput,
		getClass(r.MatchMaterno), r.MaternoInput,
		r.ID, r.NombreAPI,
		r.ID, r.PaternoAPI,
		r.ID, r.MaternoAPI, copyBtn)

	return html
}
