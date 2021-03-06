package btc

import (
	"fmt"
	"github.com/go-xorm/xorm"
	"github.com/jdcloud-bds/bds/common/httputils"
	"github.com/jdcloud-bds/bds/common/jsonrpc"
	"github.com/jdcloud-bds/bds/common/kafka"
	"github.com/jdcloud-bds/bds/common/log"
	"github.com/jdcloud-bds/bds/config"
	"github.com/jdcloud-bds/bds/service"
	model "github.com/jdcloud-bds/bds/service/model/btc"
	"github.com/xeipuuv/gojsonschema"
	"strconv"
	"strings"
	"time"
)

type SplitterConfig struct {
	Engine                     *xorm.Engine
	Consumer                   *kafka.ConsumerGroup
	Topic                      string
	DatabaseEnable             bool
	MaxBatchBlock              int
	Endpoint                   string
	User                       string
	Password                   string
	OmniEndpoint               string
	OmniUser                   string
	OmniPassword               string
	JSONSchemaFile             string
	JSONSchemaValidationEnable bool
	OmniEnable                 bool
}

type BTCSplitter struct {
	cfg                           *SplitterConfig
	remoteHandler                 *rpcHandler
	remoteHandlerOmni             *rpcHandler
	cronWorker                    *CronWorker
	jsonSchemaLoader              gojsonschema.JSONLoader
	missedBlockList               map[int64]bool
	latestSaveDataTimestamp       time.Time
	latestReceiveMessageTimestamp time.Time
}

func NewSplitter(cfg *SplitterConfig) (*BTCSplitter, error) {
	var err error
	s := new(BTCSplitter)
	s.cfg = cfg
	s.missedBlockList = make(map[int64]bool, 0)
	httpClient := httputils.NewRestClientWithBasicAuth(s.cfg.User, s.cfg.Password)
	s.remoteHandler, err = newRPCHandler(jsonrpc.New(httpClient, s.cfg.Endpoint))
	if err != nil {
		log.DetailError(err)
		return nil, err
	}
	if s.cfg.OmniEnable {
		httpClientOmni := httputils.NewRestClientWithBasicAuth(s.cfg.OmniUser, s.cfg.OmniPassword)
		s.remoteHandlerOmni, err = newRPCHandler(jsonrpc.New(httpClientOmni, s.cfg.OmniEndpoint))
		if err != nil {
			log.DetailError(err)
			return nil, err
		}
	}

	if s.cfg.JSONSchemaValidationEnable {
		f := fmt.Sprintf("file://%s", s.cfg.JSONSchemaFile)
		s.jsonSchemaLoader = gojsonschema.NewReferenceLoader(f)
	}

	s.cronWorker = NewCronWorker(s)
	err = s.cronWorker.Prepare()
	if err != nil {
		log.DetailError(err)
		return nil, err
	}

	return s, nil
}

func (s *BTCSplitter) Stop() {
	s.cronWorker.Stop()
}

func (s *BTCSplitter) CheckBlock(curBlock *BTCBlockData) (bool, int64) {
	db := service.NewDatabase(s.cfg.Engine)
	height := int64(-1)
	preBlock := make([]*model.Block, 0)
	err := db.Where("height = ?", curBlock.Block.Height-1).Find(&preBlock)
	if err != nil {
		log.DetailError(err)
		return false, height
	}
	if len(preBlock) != 1 {
		var start, end int64
		log.Warn("splitter btc: can not find previous block %d", curBlock.Block.Height-1)
		blocks := make([]*model.Block, 0)
		err = db.Desc("height").Limit(1).Find(&blocks)
		if err != nil {
			log.DetailError(err)
			return false, height
		} else {
			if len(blocks) == 0 {
				start = -1
			} else {
				start = blocks[0].Height
			}
			end = curBlock.Block.Height
			log.Debug("splitter btc: get latest block %d from database", start)
			if curBlock.Block.Height > start+int64(s.cfg.MaxBatchBlock) {
				end = start + int64(s.cfg.MaxBatchBlock)
			}
			log.Debug("splitter btc: get block range from %d to %d", start+1, end)
			err = s.remoteHandler.SendBatchBlock(start+1, end)
			if err != nil {
				log.DetailError(err)
			}
			return false, start + 1
		}

	}
	if preBlock[0].Hash != curBlock.Block.PreviousHash {
		log.Warn("splitter btc: block %d is revert", curBlock.Block.Height-1)
		err = s.remoteHandler.SendBatchBlock(preBlock[0].Height, curBlock.Block.Height)
		if err != nil {
			log.DetailError(err)
		}
		return false, preBlock[0].Height
	}
	log.Debug("splitter btc: check block %d pass", curBlock.Block.Height)
	return true, height
}

//revert block by height
func (s *BTCSplitter) RevertBlock(height int64, tx *service.Transaction) error {
	startTime := time.Now()
	//revert vout is_used, address value, miner coinbase_times
	err := revertBlock(height, tx)
	if err != nil {
		return err
	}
	//revert block table
	sql := fmt.Sprintf("DELETE FROM btc_block WHERE height = %d", height)
	affected, err := tx.Execute(sql)
	if err != nil {
		return err
	}
	log.Debug("splitter btc: revert block %d from btc_block table, affected", height, affected)
	//revert transaction table
	sql = fmt.Sprintf("DELETE FROM btc_transaction WHERE block_height = %d", height)
	affected, err = tx.Execute(sql)
	if err != nil {
		return err
	}
	log.Debug("splitter btc: revert block %d from btc_transaction table, affected", height, affected)
	//revert vin table
	sql = fmt.Sprintf("DELETE FROM btc_vin WHERE block_height = %d", height)
	affected, err = tx.Execute(sql)
	if err != nil {
		return err
	}
	log.Debug("splitter btc: revert block %d from btc_vin table, affected", height, affected)
	//revert vout table
	sql = fmt.Sprintf("DELETE FROM btc_vout WHERE block_height = %d", height)
	affected, err = tx.Execute(sql)
	if err != nil {
		return err
	}
	log.Debug("splitter btc: revert block %d from btc_vout table, affected", height, affected)
	//revert omni transaction table
	sql = fmt.Sprintf("DELETE FROM btc_omni_transaction WHERE block_height = %d", height)
	affected, err = tx.Execute(sql)
	if err != nil {
		return err
	}
	log.Debug("splitter btc: revert block %d from btc_omni_transaction table, affected", height, affected)

	elaspedTime := time.Now().Sub(startTime)
	log.Debug("splitter btc: revert block %d elasped %s", height, elaspedTime.String())
	return nil
}

func (s *BTCSplitter) Start() {
	//judge if Omni data is supported
	if s.cfg.OmniEnable {
		//make up omni data until the height of omni data is same with btc data
		err := s.MakeUpOmni()
		if err != nil {
			log.Error("splitter btc: make up omni error")
			log.DetailError(err)
			return
		}
	}

	//start kafka consumer
	err := s.cfg.Consumer.Start(s.cfg.Topic)
	if err != nil {
		log.Error("splitter btc: consumer start error")
		log.DetailError(err)
		return
	}

	log.Debug("splitter btc: consumer start topic %s", s.cfg.Topic)
	log.Debug("splitter btc: database enable is %v", s.cfg.DatabaseEnable)

	//start cron worker
	err = s.cronWorker.Start()
	if err != nil {
		log.Error("splitter btc: cron worker start error")
		log.DetailError(err)
		return
	}

	for {
		select {
		case message := <-s.cfg.Consumer.MessageChannel():
			log.Debug("splitter btc: topic %s receive data on partition %d offset %d length %d",
				message.Topic, message.Partition, message.Offset, len(message.Data))
			stats.Add(MetricReceiveMessages, 1)
			s.latestReceiveMessageTimestamp = time.Now()

		START:
			//JSON schema check
			if s.cfg.JSONSchemaValidationEnable {
				ok, err := s.jsonSchemaValid(string(message.Data))
				if err != nil {
					log.Error("splitter btc: json schema valid error")
				}
				if !ok {
					log.Warn("splitter btc: json schema valid failed")
				}
			}

			//parse block
			data, err := ParseBlock(string(message.Data))
			if err != nil {
				stats.Add(MetricParseDataError, 1)
				log.Error("splitter btc: block parse error, retry after 5s")
				log.DetailError(err)
				time.Sleep(time.Second * 5)
				goto START
			}

			//check block
			if _, ok := s.missedBlockList[data.Block.Height]; !ok {
				log.Debug("splitter btc: checking block %d", data.Block.Height)
				ok, height := s.CheckBlock(data)
				if data.Block.Height != 0 && !ok {
					log.Debug("splitter btc: block check failed, expected height %d, this block height %d", height, data.Block.Height)
					continue
				}
			} else {
				log.Debug("splitter btc: block %d is missed", data.Block.Height)
				delete(s.missedBlockList, data.Block.Height)
			}
			//save block
			if s.cfg.DatabaseEnable {
				err = s.SaveBlock(data)
				if err != nil {
					log.Error("splitter btc: block %d save error, retry after 5s", data.Block.Height)
					log.DetailError(err)
					time.Sleep(time.Second * 5)
					goto START
				} else {
					log.Info("splitter btc: block %d save success", data.Block.Height)
					s.cfg.Consumer.MarkOffset(message)
				}
			}
		}
	}
}

//check json schema
func (s *BTCSplitter) jsonSchemaValid(data string) (bool, error) {
	startTime := time.Now()
	dataLoader := gojsonschema.NewStringLoader(data)
	result, err := gojsonschema.Validate(s.jsonSchemaLoader, dataLoader)
	if err != nil {
		log.Error("splitter btc: json schema validation error")
		log.DetailError(err)
		return false, err
	}
	if !result.Valid() {
		for _, err := range result.Errors() {
			log.Error("splitter btc: data invalid %s", strings.ToLower(err.String()))
			return false, nil
		}
		stats.Add(MetricVaildationError, 1)
	} else {
		stats.Add(MetricVaildationSuccess, 1)
	}
	elaspedTime := time.Now().Sub(startTime)
	log.Debug("splitter btc: json schema validation elasped %s", elaspedTime)
	return true, nil
}

func (s *BTCSplitter) SaveBlock(data *BTCBlockData) error {
	startTime := time.Now()
	tx := service.NewTransaction(s.cfg.Engine)
	defer tx.Close()

	err := tx.Begin()
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}
	blockTemp := new(model.Block)
	blockTemp.Height = data.Block.Height
	has, err := tx.Get(blockTemp)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}

	//judge if block has been stored and if the block needs to be reverted
	if data.Block.Height == 0 {
		blocks := make([]*model.Block, 0)
		err = tx.Desc("height").Limit(1).Find(&blocks)
		if err != nil {
			log.DetailError(err)
			return err
		}
		if len(blocks) != 0 {
			log.Warn("splitter btc: block %d has been stored", data.Block.Height)
			_ = tx.Rollback()
			return nil
		}
	}
	if data.Block.Height != 0 && has {
		if blockTemp.Hash == data.Block.Hash {
			log.Warn("splitter btc: block %d has been stored", data.Block.Height)
			_ = tx.Rollback()
			return nil
		} else {
			blocks := make([]*model.Block, 0)
			err = tx.Desc("height").Limit(1).Find(&blocks)
			if err != nil {
				_ = tx.Rollback()
				log.DetailError(err)
				stats.Add(MetricDatabaseRollback, 1)
				return err
			}
			if blocks[0].Height-data.Block.Height > 6 {
				log.Warn("splitter btc: block %d reverted is too old", data.Block.Height)
				_ = tx.Rollback()
				return nil
			}
			for i := blocks[0].Height; i >= data.Block.Height; i-- {
				err = s.RevertBlock(i, tx)
				if err != nil {
					_ = tx.Rollback()
					log.DetailError(err)
					stats.Add(MetricDatabaseRollback, 1)
					return err
				}
				if s.cfg.OmniEnable {
					err = s.RevertTetherAddress(i, tx)
					if err != nil {
						_ = tx.Rollback()
						log.DetailError(err)
						stats.Add(MetricDatabaseRollback, 1)
						return err
					}
				}
				stats.Add(MetricRevertBlock, 1)
			}
		}
	}
	var affected int64
	version := data.Block.Version

	//Fill in the name of the miner
	err = GetBlockMiner(data, tx)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}

	blockList := make([]*model.Block, 0)
	blockList = append(blockList, data.Block)

	//insert block
	affected, err = tx.BatchInsert(blockList)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}
	if config.SplitterConfig.DatabaseBTCSetting.Type != "postgres" {
		sql := fmt.Sprintf("UPDATE btc_block SET version='%d' WHERE height='%d'", version, data.Block.Height)
		_, err = tx.Execute(sql)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			stats.Add(MetricDatabaseRollback, 1)
			return err
		}
	}
	log.Debug("splitter btc: block write %d rows", affected)

	//insert vouts
	affected, err = tx.BatchInsert(data.VOuts)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}
	log.Debug("splitter btc: vout write %d rows", affected)

	//get vin address and value
	err = updateVInAddressAndValue(tx, data)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}

	var txVersion []int64
	for _, v := range data.Transactions {
		txVersion = append(txVersion, v.Version)
	}

	//insert transactions
	affected, err = tx.BatchInsert(data.Transactions)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}
	if config.SplitterConfig.DatabaseBTCSetting.Type != "postgres" {
		err = updateTransactionVersion(tx, txVersion, data)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			stats.Add(MetricDatabaseRollback, 1)
			return err
		}
	}
	log.Debug("splitter btc: transaction write %d rows", affected)

	//insert vins
	affected, err = tx.BatchInsert(data.VIns)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}
	log.Debug("splitter btc: vin write %d rows", affected)

	//update address value, vout is_used, miner coinbase_times after each block
	err = UpdateBlock(data, tx)
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}

	//judge if support omni data
	if s.cfg.OmniEnable {
		//get omni data by height
		data.OmniTransactions, err = s.remoteHandlerOmni.GetOmniBlock(data.Block.Height)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			stats.Add(MetricDatabaseRollback, 1)
			return err
		}
		//insert omni transactions
		affected, err = tx.BatchInsert(data.OmniTransactions)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			stats.Add(MetricDatabaseRollback, 1)
			return err
		}
		log.Debug("splitter btc: omni write %d rows", affected)
		//update tether address value
		err = s.UpdateTetherAddress(data, tx)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			stats.Add(MetricDatabaseRollback, 1)
			return err
		}
	}
	err = tx.Commit()
	if err != nil {
		_ = tx.Rollback()
		log.DetailError(err)
		stats.Add(MetricDatabaseRollback, 1)
		return err
	}

	tx.Close()
	stats.Add(MetricDatabaseCommit, 1)
	elaspedTime := time.Now().Sub(startTime)
	s.latestSaveDataTimestamp = time.Now()
	log.Debug("splitter btc: block %d write done elasped: %s", data.Block.Height, elaspedTime.String())
	return nil
}

func (s *BTCSplitter) MakeUpOmni() error {
	db := service.NewDatabase(s.cfg.Engine)
	omniTxs := make([]*model.OmniTansaction, 0)
	blocks := make([]*model.Block, 0)
	//get max height of omni transaction
	err := db.Desc("block_height").Limit(1).Find(&omniTxs)
	if err != nil {
		log.DetailError(err)
		return err
	}
	//get max height of btc block
	err = db.Desc("height").Limit(1).Find(&blocks)
	if err != nil {
		log.DetailError(err)
		return err
	}
	var maxHeight int64
	if len(omniTxs) == 0 {
		maxHeight = MinOmniBlockHeight
	} else {
		maxHeight = omniTxs[0].BlockHeight
	}
	if len(blocks) == 0 {
		log.Debug("length of blocks is zero")
		return nil
	}
	//make up omni transaction until max height of omni is same with max height of btc block
	for i := maxHeight + 1; i <= blocks[0].Height; i++ {
		log.Debug("start block %d", i)
		tx := service.NewTransaction(s.cfg.Engine)
		err = tx.Begin()
		if err != nil {
			_ = tx.Rollback()
			tx.Close()
			log.DetailError(err)
			return err
		}
		block := new(BTCBlockData)
		block.Block = new(model.Block)
		block.Block.Height = i
		//get omni transactions from node by height
		block.OmniTransactions, err = s.remoteHandlerOmni.GetOmniBlock(i)
		if err != nil {
			_ = tx.Rollback()
			log.DetailError(err)
			tx.Close()
			return err
		}
		if len(block.OmniTransactions) != 0 {
			//insert omni transactions
			block.Block.Timestamp = block.OmniTransactions[0].Timestamp
			_, err = tx.BatchInsert(block.OmniTransactions)
			if err != nil {
				log.DetailError(err)
				_ = tx.Rollback()
				tx.Close()
				return err
			}
			//update account of tether
			err = s.UpdateTetherAddress(block, tx)
			if err != nil {
				log.DetailError(err)
				_ = tx.Rollback()
				tx.Close()
				return err
			}
		}
		err = tx.Commit()
		if err != nil {
			log.DetailError(err)
			_ = tx.Rollback()
			tx.Close()
			return err
		}
		tx.Close()
		log.Debug("block %d saved success", i)
	}
	return nil
}

func (s *BTCSplitter) UpdateTetherAddress(data *BTCBlockData, tx *service.Transaction) error {
	addressList := make([]*model.TetherAddress, 0)
	height := data.Block.Height
	addressHas := make(map[string]int64)
	//describe address witch exist in this block
	sql := fmt.Sprintf("SELECT * FROM btc_tether_address where address in (SELECT sending_address FROM btc_omni_transaction WHERE block_height=%d UNION SELECT reference_address FROM btc_omni_transaction WHERE block_height=%d)", height, height)
	result, err := tx.QueryString(sql)
	if err != nil {
		return err
	}
	for _, v := range result {
		addressInfo := new(model.TetherAddress)
		addressInfo.Address = v["address"]
		if v["address"] == "" {
			continue
		}
		addressInfo.BirthTimestamp, _ = strconv.ParseInt(v["birth_timestamp"], 10, 64)
		addressInfo.LatestTxTimestamp = data.Block.Timestamp
		addressHas[addressInfo.Address] = 1
		addressList = append(addressList, addressInfo)
	}
	//delete addresses witch exist in this block
	sql = fmt.Sprintf("DELETE FROM btc_tether_address where address in (SELECT sending_address FROM btc_omni_transaction WHERE block_height=%d UNION SELECT reference_address FROM btc_omni_transaction WHERE block_height=%d)", height, height)
	_, err = tx.Exec(sql)
	if err != nil {
		return err
	}
	addressMap := make(map[string]int64)
	for _, v := range data.OmniTransactions {
		if v.SendingAddress != "" {
			addressMap[v.SendingAddress] = 1
		}
		if v.ReferenceAddress != "" {
			addressMap[v.ReferenceAddress] = 1
		}
	}
	for k, _ := range addressMap {
		if _, has := addressHas[k]; !has {
			addressInfo := new(model.TetherAddress)
			addressInfo.Address = k
			if k == "" {
				continue
			}
			addressInfo.BirthTimestamp = data.Block.Timestamp
			addressInfo.LatestTxTimestamp = data.Block.Timestamp
			addressList = append(addressList, addressInfo)
		}
	}
	//get address balance by rpc and update address balance
	for k, _ := range addressList {
		addressList[k].Value, err = s.remoteHandlerOmni.GetTetherBalance(addressList[k].Address)
		if err != nil {
			return err
		}
	}
	//insert address witch exist in this block
	_, err = tx.BatchInsert(addressList)
	if err != nil {
		return err
	}
	return nil
}

//revert tether address table
func (s *BTCSplitter) RevertTetherAddress(height int64, tx *service.Transaction) error {
	addressList := make([]*model.TetherAddress, 0)
	//delete address witch is new in this block
	sql := fmt.Sprintf("DELETE FROM btc_tether_address WHERE birth_timestamp=(SELECT timestamp FROM btc_block WHERE height=%d)", height)
	_, err := tx.Execute(sql)
	if err != nil {
		return err
	}
	//describe address witch exist in this block
	sql = fmt.Sprintf("SELECT * FROM btc_tether_address where address in (SELECT sending_address FROM btc_omni_transaction WHERE block_height=%d UNION SELECT reference_address FROM btc_omni_transaction WHERE block_height=%d)", height, height)
	result, err := tx.QueryString(sql)
	if err != nil {
		return err
	}
	//update address balance by rpc
	for _, v := range result {
		addressInfo := new(model.TetherAddress)
		addressInfo.Address = v["address"]
		addressInfo.BirthTimestamp, _ = strconv.ParseInt(v["birth_timestamp"], 10, 64)
		addressInfo.LatestTxTimestamp, _ = strconv.ParseInt(v["latest_tx_timestamp"], 10, 64)
		addressInfo.Value, err = s.remoteHandlerOmni.GetTetherBalance(addressInfo.Address)
		if err != nil {
			return err
		}
		addressList = append(addressList, addressInfo)
	}
	_, err = tx.BatchInsert(addressList)
	if err != nil {
		return err
	}
	return nil
}
