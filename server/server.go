package server

import (
	"github.com/artpar/api2go"
	"github.com/artpar/api2go-adapter/gingonic"
	"github.com/artpar/go.uuid"
	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs"
	"github.com/artpar/rclone/fs/config"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/database"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/websockets"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/thoas/stats"
	"io/ioutil"
	"net/http"
	"github.com/graphql-go/graphql"
	"fmt"
	graphqlhandler "github.com/graphql-go/handler"
)

var TaskScheduler resource.TaskScheduler
var Stats = stats.New()

func IsStandardColumn(s string) bool {
	for _, cols := range resource.StandardColumns {
		if cols.ColumnName == s {
			return true
		}
	}
	return false
}

func executeQuery(query string, schema graphql.Schema) *graphql.Result {
	result := graphql.Do(graphql.Params{
		Schema:        schema,
		RequestString: query,
	})
	if len(result.Errors) > 0 {
		fmt.Printf("wrong result, unexpected errors: %v", result.Errors)
	}
	return result
}

func Main(boxRoot http.FileSystem, db database.DatabaseConnection) HostSwitch {

	/// Start system initialise

	log.Infof("Load config files")
	initConfig, errs := LoadConfigFiles()
	if errs != nil {
		for _, err := range errs {
			log.Errorf("Failed to load config file: %v", err)
		}
	}

	existingTables, _ := GetTablesFromWorld(db)

	allTables := MergeTables(existingTables, initConfig.Tables)

	initConfig.Tables = allTables

	// rclone config load
	config.LoadConfig()
	fs.Config.DryRun = false
	fs.Config.LogLevel = 200
	fs.Config.StatsLogLevel = 200

	initialiseResources(&initConfig, db)
	/// end system initialise

	r := gin.Default()

	r.Use(func() gin.HandlerFunc {
		return func(c *gin.Context) {
			beginning, recorder := Stats.Begin(c.Writer)
			defer Stats.End(beginning, recorder)
			c.Next()
		}
	}())

	r.GET("/statistics", func(c *gin.Context) {
		c.JSON(http.StatusOK, Stats.Data())
	})

	r.Use(CorsMiddlewareFunc)
	r.StaticFS("/static", NewSubPathFs(boxRoot, "/static"))

	r.GET("/favicon.ico", func(c *gin.Context) {

		file, err := boxRoot.Open("static/img/favicon.png")
		if err != nil {
			c.AbortWithStatus(404)
			return
		}

		fileContents, err := ioutil.ReadAll(file)
		if err != nil {
			c.AbortWithStatus(404)
			return
		}
		_, err = c.Writer.Write(fileContents)
		resource.CheckErr(err, "Failed to write favico")
	})

	r.GET("/favicon.png", func(c *gin.Context) {

		file, err := boxRoot.Open("static/img/favicon.png")
		if err != nil {
			c.AbortWithStatus(404)
			return
		}

		fileContents, err := ioutil.ReadAll(file)
		if err != nil {
			c.AbortWithStatus(404)
			return
		}
		_, err = c.Writer.Write(fileContents)
		resource.CheckErr(err, "Failed to write favico")
	})

	configStore, err := resource.NewConfigStore(db)
	jwtSecret, err := configStore.GetConfigValueFor("jwt.secret", "backend")

	if err != nil {
		u, _ := uuid.NewV4()
		newSecret := u.String()
		configStore.SetConfigValueFor("jwt.secret", newSecret, "backend")
		jwtSecret = newSecret
	}

	resource.CheckErr(err, "Failed to get config store")
	err = CheckSystemSecrets(configStore)
	resource.CheckErr(err, "Failed to initialise system secrets")

	r.GET("/config", CreateConfigHandler(configStore))

	jwtTokenIssuer, err := configStore.GetConfigValueFor("jwt.token.issuer", "backend")
	resource.CheckErr(err, "No default jwt token issuer set")
	if err != nil {
		uid, _ := uuid.NewV4()
		jwtTokenIssuer = "daptin-" + uid.String()[0:6]
		err = configStore.SetConfigValueFor("jwt.token.issuer", jwtTokenIssuer, "backend")
	}
	authMiddleware := auth.NewAuthMiddlewareBuilder(db, jwtTokenIssuer)
	auth.InitJwtMiddleware([]byte(jwtSecret), jwtTokenIssuer)
	r.Use(authMiddleware.AuthCheckMiddleware)

	cruds := make(map[string]*resource.DbResource)
	r.GET("/actions", resource.CreateGuestActionListHandler(&initConfig))

	api := api2go.NewAPIWithRouting(
		"api",
		api2go.NewStaticResolver("/"),
		gingonic.New(r),
	)

	ms := BuildMiddlewareSet(&initConfig, &cruds)
	cruds = AddResourcesToApi2Go(api, initConfig.Tables, db, &ms, configStore, cruds)

	rcloneRetries, err := configStore.GetConfigIntValueFor("rclone.retries", "backend")
	if err != nil {
		rcloneRetries = 5
		configStore.SetConfigIntValueFor("rclone.retries", rcloneRetries, "backend")
	}
	cmd.SetRetries(&rcloneRetries)

	streamProcessors := GetStreamProcessors(&initConfig, configStore, cruds)

	actionPerformers := GetActionPerformers(&initConfig, configStore, cruds)
	initConfig.ActionPerformers = actionPerformers

	AddStreamsToApi2Go(api, streamProcessors, db, &ms, configStore)

	// todo : move this somewhere and make it part of something
	actionHandlerMap := actionPerformersListToMap(actionPerformers)
	for k, _ := range cruds {
		cruds[k].ActionHandlerMap = actionHandlerMap
	}

	resource.ImportDataFiles(&initConfig, db, cruds)

	TaskScheduler = resource.NewTaskScheduler(&initConfig, cruds, configStore)
	TaskScheduler.StartTasks()

	hostSwitch := CreateSubSites(&initConfig, db, cruds, authMiddleware)

	hostSwitch.handlerMap["api"] = r
	hostSwitch.handlerMap["dashboard"] = r

	authMiddleware.SetUserCrud(cruds["user_account"])
	authMiddleware.SetUserGroupCrud(cruds["usergroup"])
	authMiddleware.SetUserUserGroupCrud(cruds["user_account_user_account_id_has_usergroup_usergroup_id"])

	fsmManager := resource.NewFsmManager(db, cruds)

	r.GET("/ping", func(c *gin.Context) {
		c.String(200, "pong")
	})

	handler := CreateJsModelHandler(&initConfig, cruds)
	metaHandler := CreateMetaHandler(&initConfig)
	blueprintHandler := CreateApiBlueprintHandler(&initConfig, cruds)
	modelHandler := CreateReclineModelHandler()
	statsHandler := CreateStatsHandler(&initConfig, cruds)
	resource.InitialiseColumnManager()
	resource.RegisterTranslations()

	graphqlSchema := MakeGraphqlSchema(&initConfig, cruds)
	//r.GET("/graphql", func(context *gin.Context) {
	//	log.Infof("graphql query: %v", context.Query("query"))
	//	result := executeQuery(context.Query("query"), *graphqlSchema)
	//	json.NewEncoder(context.Writer).Encode(result)
	//})

	graphqlHttpHandler := graphqlhandler.New(&graphqlhandler.Config{
		Schema:   graphqlSchema,
		Pretty:   true,
		GraphiQL: true,
	})

	// serve HTTP
	r.Handle("GET", "/graphql", func(c *gin.Context) {
		graphqlHttpHandler.ServeHTTP(c.Writer, c.Request)
	})
	// serve HTTP
	r.Handle("POST", "/graphql", func(c *gin.Context) {
		graphqlHttpHandler.ServeHTTP(c.Writer, c.Request)
	})
	// serve HTTP
	r.Handle("PUT", "/graphql", func(c *gin.Context) {
		graphqlHttpHandler.ServeHTTP(c.Writer, c.Request)
	})
	// serve HTTP
	r.Handle("PATCH", "/graphql", func(c *gin.Context) {
		graphqlHttpHandler.ServeHTTP(c.Writer, c.Request)
	})
	// serve HTTP
	r.Handle("DELETE", "/graphql", func(c *gin.Context) {
		graphqlHttpHandler.ServeHTTP(c.Writer, c.Request)
	})

	r.GET("/jsmodel/:typename", handler)
	r.GET("/stats/:typename", statsHandler)
	r.GET("/meta", metaHandler)
	r.GET("/apispec.raml", blueprintHandler)
	r.GET("/recline_model", modelHandler)
	r.OPTIONS("/jsmodel/:typename", handler)
	r.OPTIONS("/apispec.raml", blueprintHandler)
	r.OPTIONS("/recline_model", modelHandler)
	r.GET("/system", func(c *gin.Context) {
		c.AbortWithStatusJSON(200, Stats.Data())
	})

	r.POST("/action/:typename/:actionName", resource.CreatePostActionHandler(&initConfig, configStore, cruds, actionPerformers))
	r.GET("/action/:typename/:actionName", resource.CreatePostActionHandler(&initConfig, configStore, cruds, actionPerformers))

	r.POST("/track/start/:stateMachineId", CreateEventStartHandler(fsmManager, cruds, db))
	r.POST("/track/event/:typename/:objectStateId/:eventName", CreateEventHandler(&initConfig, fsmManager, cruds, db))

	r.POST("/site/content/load", CreateSubSiteContentHandler(&initConfig, cruds, db))
	r.POST("/site/content/store", CreateSubSiteSaveContentHandler(&initConfig, cruds, db))

	webSocketConnectionHandler := WebSocketConnectionHandlerImpl{}
	websocketServer := websockets.NewServer("/live", &webSocketConnectionHandler)

	go websocketServer.Listen(r)

	r.NoRoute(func(c *gin.Context) {
		file, err := boxRoot.Open("index.html")
		resource.CheckErr(err, "Failed to open index.html")
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
		fileContents, err := ioutil.ReadAll(file)
		_, err = c.Writer.Write(fileContents)
		resource.CheckErr(err, "Failed to write index html")
	})

	//r.Run(fmt.Sprintf(":%v", *port))
	CleanUpConfigFiles()

	return hostSwitch

}
func initialiseResources(initConfig *resource.CmsConfig, db database.DatabaseConnection) {
	resource.CheckRelations(initConfig)
	resource.CheckAuditTables(initConfig)
	//AddStateMachines(&initConfig, db)
	tx, errb := db.Beginx()
	//_, errb := db.Exec("begin")
	resource.CheckErr(errb, "Failed to begin transaction")

	resource.CheckAllTableStatus(initConfig, db, tx)
	resource.CreateRelations(initConfig, tx)
	resource.CreateUniqueConstraints(initConfig, tx)
	resource.CreateIndexes(initConfig, tx)
	resource.UpdateWorldTable(initConfig, tx)
	errc := tx.Commit()
	resource.CheckErr(errc, "Failed to commit transaction")

	resource.UpdateStateMachineDescriptions(initConfig, db)
	resource.UpdateExchanges(initConfig, db)
	resource.UpdateStreams(initConfig, db)
	resource.UpdateMarketplaces(initConfig, db)
	err := resource.UpdateTasksData(initConfig, db)
	resource.CheckErr(err, "Failed to  update cron jobs")
	resource.UpdateStandardData(initConfig, db)

	err = resource.UpdateActionTable(initConfig, db)
	resource.CheckErr(err, "Failed to update action table")

}
func actionPerformersListToMap(interfaces []resource.ActionPerformerInterface) map[string]resource.ActionPerformerInterface {
	m := make(map[string]resource.ActionPerformerInterface)

	for _, api := range interfaces {
		m[api.Name()] = api
	}
	return m
}

func MergeTables(existingTables []resource.TableInfo, initConfigTables []resource.TableInfo) []resource.TableInfo {
	allTables := make([]resource.TableInfo, 0)
	existingTablesMap := make(map[string]bool)

	for j, existableTable := range existingTables {
		existingTablesMap[existableTable.TableName] = true
		var isBeingModified = false
		var indexBeingModified = -1

		for i, newTable := range initConfigTables {
			if newTable.TableName == existableTable.TableName {
				isBeingModified = true
				indexBeingModified = i
				break
			}
		}

		if isBeingModified {
			log.Debugf("Table %s is being modified", existableTable.TableName)
			tableBeingModified := initConfigTables[indexBeingModified]

			if len(tableBeingModified.Columns) > 0 {

				for _, newColumnDef := range tableBeingModified.Columns {
					columnAlreadyExist := false
					colIndex := -1
					for i, existingColumn := range existableTable.Columns {
						//log.Infof("Table column old/new [%v][%v] == [%v][%v] @ %v", tableBeingModified.TableName, newColumnDef.Name, existableTable.TableName, existingColumn.Name, i)
						if existingColumn.Name == newColumnDef.Name || existingColumn.ColumnName == newColumnDef.ColumnName {
							columnAlreadyExist = true
							colIndex = i
							break
						}
					}
					//log.Infof("Decide for table column [%v][%v] @ index: %v [%v]", tableBeingModified.TableName, newColumnDef.Name, colIndex, columnAlreadyExist)
					if columnAlreadyExist {
						//log.Infof("Modifying existing columns[%v][%v] is not supported at present. not sure what would break. and alter query isnt being run currently.", existableTable.TableName, newColumnDef.Name);

						existableTable.Columns[colIndex].DefaultValue = newColumnDef.DefaultValue
						existableTable.Columns[colIndex].ExcludeFromApi = newColumnDef.ExcludeFromApi
						existableTable.Columns[colIndex].IsIndexed = newColumnDef.IsIndexed
						existableTable.Columns[colIndex].IsNullable = newColumnDef.IsNullable
						existableTable.Columns[colIndex].ColumnType = newColumnDef.ColumnType
						existableTable.Columns[colIndex].Options = newColumnDef.Options
						existableTable.Columns[colIndex].DataType = newColumnDef.DataType
						existableTable.Columns[colIndex].ColumnType = newColumnDef.ColumnType

					} else {
						existableTable.Columns = append(existableTable.Columns, newColumnDef)
					}
				}

			}
			if len(tableBeingModified.Relations) > 0 {

				existingRelations := existableTable.Relations
				relMap := make(map[string]bool)
				for _, rel := range existingRelations {
					relMap[rel.Hash()] = true
				}

				for _, newRel := range tableBeingModified.Relations {

					_, ok := relMap[newRel.Hash()]
					if !ok {
						existableTable.AddRelation(newRel)
					}
				}
			}
			existableTable.DefaultGroups = tableBeingModified.DefaultGroups
			existableTable.Conformations = tableBeingModified.Conformations
			existableTable.Validations = tableBeingModified.Validations
			existingTables[j] = existableTable
		} else {
			//log.Infof("Table %s is not being modified", existableTable.TableName)
		}
		allTables = append(allTables, existableTable)
	}

	for _, newTable := range initConfigTables {
		if existingTablesMap[newTable.TableName] {
			continue
		}
		allTables = append(allTables, newTable)
	}

	return allTables

}

func NewSubPathFs(system http.FileSystem, s string) http.FileSystem {
	return &SubPathFs{system: system, subPath: s}
}

type SubPathFs struct {
	system  http.FileSystem
	subPath string
}

func (spf *SubPathFs) Open(name string) (http.File, error) {
	//log.Infof("Service file from static path: %s/%s", spf.subPath, name)
	return spf.system.Open(spf.subPath + name)
}

type WebSocketConnectionHandlerImpl struct {
}

func (wsch *WebSocketConnectionHandlerImpl) MessageFromClient(message websockets.WebSocketPayload, request *http.Request) {

}

func AddStreamsToApi2Go(api *api2go.API, processors []*resource.StreamProcessor, db database.DatabaseConnection, middlewareSet *resource.MiddlewareSet, configStore *resource.ConfigStore) {

	for _, processor := range processors {

		contract := processor.GetContract()
		model := api2go.NewApi2GoModel(contract.StreamName, contract.Columns, 0, nil)
		api.AddResource(model, processor)

	}

}

func GetStreamProcessors(config *resource.CmsConfig, store *resource.ConfigStore, cruds map[string]*resource.DbResource) []*resource.StreamProcessor {

	allProcessors := make([]*resource.StreamProcessor, 0)

	for _, streamContract := range config.Streams {

		streamProcessor := resource.NewStreamProcessor(streamContract, cruds)
		allProcessors = append(allProcessors, streamProcessor)

	}

	return allProcessors

}
