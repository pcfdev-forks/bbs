package main

import (
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/auctioneer"
	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/db"
	etcddb "github.com/cloudfoundry-incubator/bbs/db/etcd"
	"github.com/cloudfoundry-incubator/bbs/db/migrations"
	"github.com/cloudfoundry-incubator/bbs/db/sqldb"
	"github.com/cloudfoundry-incubator/bbs/encryption"
	"github.com/cloudfoundry-incubator/bbs/encryptor"
	"github.com/cloudfoundry-incubator/bbs/events"
	"github.com/cloudfoundry-incubator/bbs/format"
	"github.com/cloudfoundry-incubator/bbs/guidprovider"
	"github.com/cloudfoundry-incubator/bbs/handlers"
	"github.com/cloudfoundry-incubator/bbs/metrics"
	"github.com/cloudfoundry-incubator/bbs/migration"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/bbs/taskworkpool"
	"github.com/cloudfoundry-incubator/cf-debug-server"
	"github.com/cloudfoundry-incubator/cf-lager"
	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/locket"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry/dropsonde"
	etcdclient "github.com/coreos/go-etcd/etcd"
	"github.com/hashicorp/consul/api"
	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
)

var listenAddress = flag.String(
	"listenAddress",
	"",
	"The host:port that the server is bound to.",
)

var requireSSL = flag.Bool(
	"requireSSL",
	false,
	"whether the bbs server should require ssl-secured communication",
)

var caFile = flag.String(
	"caFile",
	"",
	"the certificate authority public key file to use with ssl authentication",
)

var certFile = flag.String(
	"certFile",
	"",
	"the public key file to use with ssl authentication",
)

var keyFile = flag.String(
	"keyFile",
	"",
	"the private key file to use with ssl authentication",
)

var advertiseURL = flag.String(
	"advertiseURL",
	"",
	"The URL to advertise to clients",
)

var communicationTimeout = flag.Duration(
	"communicationTimeout",
	10*time.Second,
	"Timeout applied to all HTTP requests.",
)

var auctioneerAddress = flag.String(
	"auctioneerAddress",
	"",
	"The address to the auctioneer api server",
)

var sessionName = flag.String(
	"sessionName",
	"bbs",
	"consul session name",
)

var consulCluster = flag.String(
	"consulCluster",
	"",
	"comma-separated list of consul server URLs (scheme://ip:port)",
)

var lockTTL = flag.Duration(
	"lockTTL",
	locket.LockTTL,
	"TTL for service lock",
)

var lockRetryInterval = flag.Duration(
	"lockRetryInterval",
	locket.RetryInterval,
	"interval to wait before retrying a failed lock acquisition",
)

var reportInterval = flag.Duration(
	"metricsReportInterval",
	time.Minute,
	"interval on which to report metrics",
)

var dropsondePort = flag.Int(
	"dropsondePort",
	3457,
	"port the local metron agent is listening on",
)

var convergenceWorkers = flag.Int(
	"convergenceWorkers",
	20,
	"Max concurrency for convergence",
)

var updateWorkers = flag.Int(
	"updateWorkers",
	1000,
	"Max concurrency for etcd updates in a single request",
)

var taskCallBackWorkers = flag.Int(
	"taskCallBackWorkers",
	1000,
	"Max concurrency for task callback requests",
)

var desiredLRPCreationTimeout = flag.Duration(
	"desiredLRPCreationTimeout",
	1*time.Minute,
	"Expected maximum time to create all components of a desired LRP",
)

var databaseConnectionString = flag.String(
	"databaseConnectionString",
	"",
	"SQL database connection string",
)

var maxDatabaseConnections = flag.Int(
	"maxDatabaseConnections",
	200,
	"Max numbers of SQL database connections",
)

var databaseDriver = flag.String(
	"databaseDriver",
	"mysql",
	"SQL database driver name",
)

const (
	dropsondeOrigin           = "bbs"
	bbsWatchRetryWaitDuration = 3 * time.Second
)

func main() {
	cf_debug_server.AddFlags(flag.CommandLine)
	cf_lager.AddFlags(flag.CommandLine)
	etcdFlags := AddETCDFlags(flag.CommandLine)
	encryptionFlags := encryption.AddEncryptionFlags(flag.CommandLine)

	flag.Parse()

	cf_http.Initialize(*communicationTimeout)

	logger, reconfigurableSink := cf_lager.New("bbs")
	logger.Info("starting")

	initializeDropsonde(logger)

	clock := clock.NewClock()

	consulClient, err := consuladapter.NewClientFromUrl(*consulCluster)
	if err != nil {
		logger.Fatal("new-consul-client-failed", err)
	}

	serviceClient := bbs.NewServiceClient(consulClient, clock)

	maintainer := initializeLockMaintainer(logger, serviceClient)

	_, portString, err := net.SplitHostPort(*listenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}
	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	registrationRunner := initializeRegistrationRunner(logger, consulClient, portNum, clock)

	cbWorkPool := taskworkpool.New(logger, *taskCallBackWorkers, taskworkpool.HandleCompletedTask)

	etcdOptions, err := etcdFlags.Validate()
	if err != nil {
		logger.Fatal("etcd-validation-failed", err)
	}
	storeClient := initializeEtcdStoreClient(logger, etcdOptions)

	key, keys, err := encryptionFlags.Parse()
	if err != nil {
		logger.Fatal("cannot-setup-encryption", err)
	}
	keyManager, err := encryption.NewKeyManager(key, keys)
	if err != nil {
		logger.Fatal("cannot-setup-encryption", err)
	}
	cryptor := encryption.NewCryptor(keyManager, rand.Reader)

	etcdDB := initializeEtcdDB(logger, cryptor, storeClient, cbWorkPool, serviceClient, *desiredLRPCreationTimeout)

	var activeDB db.DB
	var sqlDB *sqldb.SQLDB
	activeDB = etcdDB

	// If SQL database info is passed in, use SQL instead of ETCD
	if *databaseDriver != "" && *databaseConnectionString != "" {
		sqlConn, err := sql.Open(*databaseDriver, *databaseConnectionString)
		if err != nil {
			logger.Fatal("failed-to-open-sql", err)
		}
		sqlConn.SetMaxOpenConns(*maxDatabaseConnections)

		err = sqlConn.Ping()
		if err != nil {
			logger.Fatal("sql-failed-to-connect", err)
		}

		sqlDB = sqldb.NewSQLDB(sqlConn, *convergenceWorkers, *updateWorkers, format.ENCRYPTED_PROTO, cryptor, guidprovider.DefaultGuidProvider, clock)
		err = sqlDB.CreateInitialSchema(logger)
		if err != nil {
			logger.Fatal("sql-failed-create-initial-schema", err)
		}
		activeDB = sqlDB
	}

	encryptor := encryptor.New(logger, activeDB, keyManager, cryptor, clock)

	migrationsDone := make(chan struct{})

	migrationManager := migration.NewManager(logger,
		etcdDB,
		cryptor,
		storeClient,
		migrations.Migrations,
		migrationsDone,
		clock,
	)

	desiredHub := events.NewHub()
	actualHub := events.NewHub()

	repClientFactory := rep.NewClientFactory(cf_http.NewClient(), cf_http.NewClient())
	auctioneerClient := initializeAuctioneerClient(logger)

	handler := handlers.New(
		logger,
		*updateWorkers,
		*convergenceWorkers,
		activeDB,
		desiredHub,
		actualHub,
		cbWorkPool,
		serviceClient,
		auctioneerClient,
		repClientFactory,
		migrationsDone,
	)

	metricsNotifier := metrics.NewPeriodicMetronNotifier(
		logger,
		*reportInterval,
		etcdOptions,
		clock,
	)

	var server ifrit.Runner
	if *requireSSL {
		tlsConfig, err := cf_http.NewTLSConfig(*certFile, *keyFile, *caFile)
		if err != nil {
			logger.Fatal("tls-configuration-failed", err)
		}
		server = http_server.NewTLSServer(*listenAddress, handler, tlsConfig)
	} else {
		server = http_server.New(*listenAddress, handler)
	}

	members := grouper.Members{
		{"lock-maintainer", maintainer},
		{"workpool", cbWorkPool},
		{"server", server},
		{"migration-manager", migrationManager},
		{"encryptor", encryptor},
		{"hub-maintainer", hubMaintainer(logger, desiredHub, actualHub)},
		{"metrics", *metricsNotifier},
		{"registration-runner", registrationRunner},
	}

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func hubMaintainer(logger lager.Logger, desiredHub, actualHub events.Hub) ifrit.RunFunc {
	return func(signals <-chan os.Signal, ready chan<- struct{}) error {
		logger := logger.Session("hub-maintainer")
		close(ready)
		logger.Info("started")
		defer logger.Info("finished")

		<-signals
		err := desiredHub.Close()
		if err != nil {
			logger.Error("error-closing-desired-hub", err)
		}
		err = actualHub.Close()
		if err != nil {
			logger.Error("error-closing-actual-hub", err)
		}
		return nil
	}
}

func initializeRegistrationRunner(
	logger lager.Logger,
	consulClient consuladapter.Client,
	port int,
	clock clock.Clock) ifrit.Runner {
	registration := &api.AgentServiceRegistration{
		Name: "bbs",
		Port: port,
		Check: &api.AgentServiceCheck{
			TTL: "3s",
		},
	}
	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}

func initializeLockMaintainer(logger lager.Logger, serviceClient bbs.ServiceClient) ifrit.Runner {
	uuid, err := uuid.NewV4()
	if err != nil {
		logger.Fatal("Couldn't generate uuid", err)
	}

	if *advertiseURL == "" {
		logger.Fatal("Advertise URL must be specified", nil)
	}

	bbsPresence := models.NewBBSPresence(uuid.String(), *advertiseURL)
	lockMaintainer, err := serviceClient.NewBBSLockRunner(logger, &bbsPresence, *lockRetryInterval, *lockTTL)
	if err != nil {
		logger.Fatal("Couldn't create lock maintainer", err)
	}

	return lockMaintainer
}

func initializeAuctioneerClient(logger lager.Logger) auctioneer.Client {
	if *auctioneerAddress == "" {
		logger.Fatal("auctioneer-address-validation-failed", errors.New("auctioneerAddress is required"))
	}
	return auctioneer.NewClient(*auctioneerAddress)
}

func initializeDropsonde(logger lager.Logger) {
	dropsondeDestination := fmt.Sprint("localhost:", *dropsondePort)
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed-to-initialize-dropsonde", err)
	}
}

func initializeEtcdDB(
	logger lager.Logger,
	cryptor encryption.Cryptor,
	storeClient etcddb.StoreClient,
	cbClient taskworkpool.TaskCompletionClient,
	serviceClient bbs.ServiceClient,
	desiredLRPCreationMaxTime time.Duration,
) *etcddb.ETCDDB {
	return etcddb.NewETCD(
		format.ENCRYPTED_PROTO,
		*convergenceWorkers,
		*updateWorkers,
		desiredLRPCreationMaxTime,
		cryptor,
		storeClient,
		clock.NewClock(),
	)
}

func initializeEtcdStoreClient(logger lager.Logger, etcdOptions *etcddb.ETCDOptions) etcddb.StoreClient {
	var etcdClient *etcdclient.Client
	var tr *http.Transport

	if etcdOptions.IsSSL {
		if etcdOptions.CertFile == "" || etcdOptions.KeyFile == "" {
			logger.Fatal("failed-to-construct-etcd-tls-client", errors.New("Require both cert and key path"))
		}

		var err error
		etcdClient, err = etcdclient.NewTLSClient(etcdOptions.ClusterUrls, etcdOptions.CertFile, etcdOptions.KeyFile, etcdOptions.CAFile)
		if err != nil {
			logger.Fatal("failed-to-construct-etcd-tls-client", err)
		}

		tlsCert, err := tls.LoadX509KeyPair(etcdOptions.CertFile, etcdOptions.KeyFile)
		if err != nil {
			logger.Fatal("failed-to-construct-etcd-tls-client", err)
		}

		tlsConfig := &tls.Config{
			Certificates:       []tls.Certificate{tlsCert},
			InsecureSkipVerify: true,
			ClientSessionCache: tls.NewLRUClientSessionCache(etcdOptions.ClientSessionCacheSize),
		}
		tr = &http.Transport{
			TLSClientConfig:     tlsConfig,
			Dial:                etcdClient.DefaultDial,
			MaxIdleConnsPerHost: etcdOptions.MaxIdleConnsPerHost,
		}
		etcdClient.SetTransport(tr)
		etcdClient.AddRootCA(etcdOptions.CAFile)
	} else {
		etcdClient = etcdclient.NewClient(etcdOptions.ClusterUrls)
	}
	etcdClient.SetConsistency(etcdclient.STRONG_CONSISTENCY)

	return etcddb.NewStoreClient(etcdClient)
}
