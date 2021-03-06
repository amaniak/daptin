package server

import (
	"github.com/artpar/api2go"
	"github.com/daptin/daptin/server/resource"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"path/filepath"
)

//import "github.com/daptin/daptin/datastore"

func CreateConfigHandler(configStore *resource.ConfigStore) func(context *gin.Context) {

	return func(c *gin.Context) {
		webConfig := configStore.GetWebConfig()
		c.JSON(200, webConfig)
	}
}

// Load config files which have the naming of the form schema_*_daptin.json/yaml
func LoadConfigFiles() (resource.CmsConfig, []error) {

	var err error

	errs := make([]error, 0)
	var globalInitConfig resource.CmsConfig
	globalInitConfig = resource.CmsConfig{
		Tables:                   make([]resource.TableInfo, 0),
		Relations:                make([]api2go.TableRelation, 0),
		Imports:                  make([]resource.DataFileImport, 0),
		Actions:                  make([]resource.Action, 0),
		StateMachineDescriptions: make([]resource.LoopbookFsmDescription, 0),
		Streams:                  make([]resource.StreamContract, 0),
		Marketplaces:             make([]resource.Marketplace, 0),
	}

	globalInitConfig.Tables = append(globalInitConfig.Tables, resource.StandardTables...)
	globalInitConfig.Tasks = append(globalInitConfig.Tasks, resource.StandardTasks...)
	globalInitConfig.Actions = append(globalInitConfig.Actions, resource.SystemActions...)
	globalInitConfig.Streams = append(globalInitConfig.Streams, resource.StandardStreams...)
	globalInitConfig.Marketplaces = append(globalInitConfig.Marketplaces, resource.StandardMarketplaces...)
	globalInitConfig.StateMachineDescriptions = append(globalInitConfig.StateMachineDescriptions, resource.SystemSmds...)
	globalInitConfig.ExchangeContracts = append(globalInitConfig.ExchangeContracts, resource.SystemExchanges...)

	files, err := filepath.Glob("schema_*.*")
	log.Infof("Found files to load: %v", files)

	if err != nil {
		errs = append(errs, err)
		return globalInitConfig, errs
	}

	for _, fileName := range files {
		log.Infof("Process file: %v", fileName)

		viper.SetConfigFile(fileName)

		err = viper.ReadInConfig()
		if err != nil {
			errs = append(errs, err)
		}

		initConfig := resource.CmsConfig{}
		err = viper.Unmarshal(&initConfig)

		if err != nil {
			errs = append(errs, err)
			continue
		}

		globalInitConfig.Tables = append(globalInitConfig.Tables, initConfig.Tables...)

		//globalInitConfig.Relations = append(globalInitConfig.Relations, initConfig.Relations...)
		globalInitConfig.AddRelations(initConfig.Relations...)

		globalInitConfig.Imports = append(globalInitConfig.Imports, initConfig.Imports...)
		globalInitConfig.Streams = append(globalInitConfig.Streams, initConfig.Streams...)
		globalInitConfig.Marketplaces = append(globalInitConfig.Marketplaces, initConfig.Marketplaces...)
		globalInitConfig.Tasks = append(globalInitConfig.Tasks, initConfig.Tasks...)
		globalInitConfig.Actions = append(globalInitConfig.Actions, initConfig.Actions...)
		globalInitConfig.StateMachineDescriptions = append(globalInitConfig.StateMachineDescriptions, initConfig.StateMachineDescriptions...)
		globalInitConfig.ExchangeContracts = append(globalInitConfig.ExchangeContracts, initConfig.ExchangeContracts...)


		for _, action := range initConfig.Actions {
			log.Infof("Action [%v][%v]", fileName, action.Name)
		}

		for _, marketplace := range initConfig.Marketplaces {
			log.Infof("Marketplace [%v][%v]", fileName, marketplace.Endpoint)
		}

		for _, smd := range initConfig.StateMachineDescriptions {
			log.Infof("Marketplace [%v][%v]", fileName, smd.Name, smd.InitialState)
		}

		//log.Infof("File added to config, deleting %v", fileName)

	}

	return globalInitConfig, errs

}
