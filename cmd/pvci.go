package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/txn2/pvci"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ipEnv               = getEnv("IP", "127.0.0.1")
	portEnv             = getEnv("PORT", "8070")
	metricsPortEnv      = getEnv("METRICS_PORT", "2112")
	modeEnv             = getEnv("MODE", "release")
	httpReadTimeoutEnv  = getEnv("HTTP_READ_TIMEOUT", "10")
	httpWriteTimeoutEnv = getEnv("HTTP_WRITE_TIMEOUT", "10")
)

var Version = "0.0.0"
var Service = "pvci"

func main() {
	httpReadTimeoutInt, err := strconv.Atoi(httpReadTimeoutEnv)
	if err != nil {
		fmt.Println("Parsing error, readTimeout must be an integer in seconds.")
		os.Exit(1)
	}

	httpWriteTimeoutInt, err := strconv.Atoi(httpWriteTimeoutEnv)
	if err != nil {
		fmt.Println("Parsing error, readTimeout must be an integer in seconds.")
		os.Exit(1)
	}

	var (
		ip               = flag.String("ip", ipEnv, "Server IP address to bind to.")
		port             = flag.String("port", portEnv, "Server port.")
		metricsPort      = flag.String("metricsPort", metricsPortEnv, "Metrics port.")
		mode             = flag.String("mode", modeEnv, "debug or release")
		httpReadTimeout  = flag.Int("httpReadTimeout", httpReadTimeoutInt, "HTTP read timeout")
		httpWriteTimeout = flag.Int("httpWriteTimeout", httpWriteTimeoutInt, "HTTP write timeout")
	)
	flag.Parse()

	// add some useful info to metrics
	promauto.NewCounter(prometheus.CounterOpts{
		Namespace: Service + "_service",
		Name:      "info",
		ConstLabels: prometheus.Labels{
			"go_version": runtime.Version(),
			"version":    Version,
			"mode":       *mode,
			"service":    Service,
		},
	}).Inc()

	zapCfg := zap.NewProductionConfig()
	logger, err := zapCfg.Build()
	if err != nil {
		fmt.Printf("Can not build logger: %s\n", err.Error())
		os.Exit(1)
	}

	logger.Info("Starting "+Service+" API Server",
		zap.String("version", Version),
		zap.String("type", "server_startup"),
		zap.String("mode", *mode),
		zap.String("port", *port),
		zap.String("ip", *ip),
	)

	// Kubernetes
	kubeconfig := filepath.Join(
		os.Getenv("HOME"), ".kube", "config",
	)

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatal("Unable to load configuration")
		}
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal("unable to kubernetes.NewForConfig", zap.Error(err))
	}

	// get api
	api, err := pvci.NewApi(&pvci.Config{
		Service: Service,
		Version: Version,
		Log:     logger,
		Cs:      cs,
	})
	if err != nil {
		logger.Fatal("Error getting API.", zap.Error(err))
	}

	gin.SetMode(gin.ReleaseMode)
	if *mode == "debug" {
		gin.SetMode(gin.DebugMode)
	}

	// gin router
	r := gin.New()

	// gin zap logger middleware
	r.Use(ginzap.Ginzap(logger, time.RFC3339, true))

	// gin prometheus middleware
	p := ginprometheus.NewPrometheus("http_gin")

	// loop through request and replace values with key names
	// to prevent key explosion in prom
	p.ReqCntURLLabelMappingFn = func(c *gin.Context) string {
		url := c.Request.URL.Path
		for _, p := range c.Params {
			url = strings.Replace(url, p.Value, ":"+p.Key, 1)
		}
		return url
	}
	p.Use(r)

	// status
	r.GET("/", api.OkHandler(Version, *mode, Service))

	// get bucket size
	r.POST("/size", api.GetSizeHandler())

	// create pvc
	r.POST("/create", api.CreatePVCHandler())

	// get status
	r.POST("/status", api.GetStatusHandler())

	// cleanup
	r.POST("/cleanup", api.CleanupHandler())

	// set mode to ROX
	r.POST("/mode/rox", api.SetModeHandler([]corev1.PersistentVolumeAccessMode{v1.ReadOnlyMany}))

	// set mode to RWO
	r.POST("/mode/rwo", api.SetModeHandler([]corev1.PersistentVolumeAccessMode{v1.ReadWriteOnce}))

	// delete
	r.POST("/delete", api.DeleteHandler())

	// metrics server (run in go routine)
	go func() {
		http.Handle("/metrics", promhttp.Handler())

		logger.Info("Starting "+Service+" Metrics Server",
			zap.String("version", Version),
			zap.String("type", "metrics_startup"),
			zap.String("port", *metricsPort),
			zap.String("ip", *ip),
		)

		err = http.ListenAndServe(*ip+":"+*metricsPort, nil)
		if err != nil {
			logger.Fatal("Error Starting "+Service+" Metrics Server", zap.Error(err))
			os.Exit(1)
		}
	}()

	s := &http.Server{
		Addr:           *ip + ":" + *port,
		Handler:        r,
		ReadTimeout:    time.Duration(*httpReadTimeout) * time.Second,
		WriteTimeout:   time.Duration(*httpWriteTimeout) * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	err = s.ListenAndServe()
	if err != nil {
		logger.Fatal(err.Error())
	}

}

// getEnv gets an environment variable or sets a default if
// one does not exist.
func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return fallback
	}

	return value
}
