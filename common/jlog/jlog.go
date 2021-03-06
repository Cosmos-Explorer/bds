package log

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const (
	LongLogFormat    = "%s %s [%s] - %s\n"
	ShortLogFormat   = "%s %s - %s\n"
	PlainLogFormat   = "%s %s\n"
	DateFormat       = "2006-01-02 15:04:05.000"
	FileWriterTag    = "FILE"
	ConsoleWriterTag = "CONSOLE"
	HTTPWriterTag    = "HTTP"
)

var (
	//	currentPath     = pathutils.GetCurrentPath()
	consoleWriter   = new(ConsoleWriter)
	loggerInstances = make(map[string]Logger)
)

func getSource(l int) string {
	pc, _, l, ok := runtime.Caller(l)
	s := ""
	if ok {
		s = fmt.Sprintf("%s:%d", runtime.FuncForPC(pc).Name(), l)
	}
	return s
}

type LogRecord interface {
	Format(format string) string

	GetLevel() Level

	GetCreated() time.Time
}

type DefaultLogRecord struct {
	Level      Level
	RecordTime time.Time
	Source     string
	LogName    string
	Content    string
}

func (r *DefaultLogRecord) GetLevel() Level {
	return r.Level
}

func (r *DefaultLogRecord) GetCreated() time.Time {
	return r.RecordTime
}

func (r *DefaultLogRecord) Format(format string) string {
	switch format {
	case "LONG":
		return fmt.Sprintf(LongLogFormat, r.RecordTime.Format(DateFormat), r.Level, r.Source, r.Content)
	case "PLAIN":
		return fmt.Sprintf(PlainLogFormat, r.RecordTime.Format(DateFormat), r.Content)
	}
	return fmt.Sprintf(ShortLogFormat, r.RecordTime.Format(DateFormat), r.Level, r.Content)
}

type Appender interface {
	AddWriter(name string, writer LogWriter)

	Log(level Level, name string, content interface{}, args ...interface{})

	Close()
}

type LogWriter interface {
	Log(record LogRecord)

	Open() error

	Close()

	GetCallerLevelSkip() uint
}

type SyncAppender struct {
	writers map[string][]LogWriter
}

func (s *SyncAppender) AddWriter(name string, writer LogWriter) {
	s.writers[name] = append(s.writers[name], writer)
}

func (s *SyncAppender) Log(level Level, name string, content interface{}, args ...interface{}) {
	text := "%+v"
	switch value := content.(type) {
	case string:
		text = fmt.Sprintf(value, args...)
	default:
		text = fmt.Sprintf(text, value)
	}
	record := &DefaultLogRecord{
		Level:      level,
		RecordTime: time.Now(),
		Source:     "",
		LogName:    name,
		Content:    text,
	}
	for _, writers := range s.writers {
		for _, writer := range writers {
			if level > 0 {
				record.Source = getSource(int(writer.GetCallerLevelSkip()))
			}
			writer.Log(record)
		}
	}
}

func (s *SyncAppender) Close() {
	for _, writers := range s.writers {
		for _, writer := range writers {
			writer.Close()
		}
	}
}

type JLogger struct {
	name     string
	level    int8
	appender Appender
	parent   *JLogger
}

func (l *JLogger) Fatal(content interface{}, args ...interface{}) {
	l.appender.Log(FATAL, l.name, content, args...)
}

func (l *JLogger) IsErrorEnable() bool {
	return l.level >= ERROR
}

func (l *JLogger) Error(content interface{}, args ...interface{}) {
	if l.IsErrorEnable() {
		l.appender.Log(ERROR, l.name, content, args...)
	}
}

func (l *JLogger) IsWarnEnable() bool {
	return l.level >= WARN
}

func (l *JLogger) Warn(content interface{}, args ...interface{}) {
	if l.IsWarnEnable() {
		l.appender.Log(WARN, l.name, content, args...)
	}
}

func (l *JLogger) IsInfoEnable() bool {
	return l.level >= INFO
}

func (l *JLogger) Info(content interface{}, args ...interface{}) {
	if l.IsInfoEnable() {
		l.appender.Log(INFO, l.name, content, args...)
	}
}

func (l *JLogger) IsDebugEnable() bool {
	return l.level >= DEBUG
}

func (l *JLogger) Debug(content interface{}, args ...interface{}) {
	if l.IsDebugEnable() {
		l.appender.Log(DEBUG, l.name, content, args...)
	}
}

func (l *JLogger) Log(content interface{}, args ...interface{}) {
	l.appender.Log(-1, l.name, content, args...)
}

func (l *JLogger) Close() {
	l.appender.Close()
}

func CreateJLogger(name string) Logger {
	var logger Logger
	var err error
	if name != ROOT {
		logger = loggerInstances[name]
		if logger == nil {
			logger, err = createLogger(name, RootLogger)
			if err != nil {
				fmt.Println(err)
			}
			loggerInstances[name] = logger
		}
	} else {
		logger, err = createLogger(ROOT, nil)
		if err != nil {
			fmt.Println(err)
		}
	}
	return logger
}

func createLogger(name string, parent Logger) (Logger, error) {
	if DebugFlag {
		fmt.Printf("[Create Logger]\n")
		fmt.Printf("Name: %v\n", name)
		fmt.Printf("Parent: %v\n", parent)
		fmt.Printf("%v\n", cfg[name].Writers)
		fmt.Printf("%v\n", cfg[name].CallerLevelSkip)
		fmt.Printf("%v\n", cfg[name].Level)
		fmt.Printf("%v\n", cfg[name].File)
		fmt.Printf("%v\n", cfg[name].MaxSize)
		fmt.Printf("%v\n", cfg[name].Daily)
	}
	logger := new(JLogger)
	logger.name = name
	loggerLevel := cfg[name].Level
	if parent != nil {
		switch value := parent.(type) {
		case *JLogger:
			logger.parent = value
		default:
		}
	}

	if DebugFlag {
		fmt.Printf("JLogger name: %v\n", name)
		fmt.Printf("JLogger level: %v\n", loggerLevel)
	}
	if loggerLevel == "" && logger.parent != nil {
		logger.level = logger.parent.level
	} else {
		switch strings.TrimSpace(strings.ToUpper(loggerLevel)) {
		case "FATAL":
			logger.level = FATAL
		case "ERROR":
			logger.level = ERROR
		case "WARN":
			logger.level = WARN
		case "INFO":
			logger.level = INFO
		case "DEBUG":
			logger.level = DEBUG
		default:
			logger.level = -1
		}
	}

	if DebugFlag {
		fmt.Printf("[Create SyncAppender]\n")
		fmt.Printf("%v\n", logger.parent)
	}

	if cfg[name].Writers == "" && logger.parent != nil {
		logger.appender = logger.parent.appender
	} else {
		appender := new(SyncAppender)
		appender.writers = make(map[string][]LogWriter)
		logger.appender = appender

		for _, writer := range strings.Split(cfg[name].Writers, ",") {
			flag := false
			if DebugFlag {
				fmt.Printf("Writer: %v\n", writer)
			}
			switch writer {
			case FileWriterTag:
				writer, err := NewFileLogWriter(name)
				if err != nil {
					flag = true
				} else {
					appender.AddWriter(name, writer)
				}
			case HTTPWriterTag:
				writer, err := NewHTTPLogWriter(name)
				if err != nil {
					flag = true
				} else {
					appender.AddWriter(name, writer)
				}
			case ConsoleWriterTag:
				writer, err := NewConsoleLogWriter(name)
				if err != nil {
					return logger, err
				}
				appender.AddWriter(name, writer)
			}

			if flag {
				writer, err := NewConsoleLogWriter(name)
				if err != nil {
					return logger, err
				}
				appender.AddWriter(name, writer)
			}
		}
	}
	return logger, nil
}

func Registry(configFile string) {
	LoadFromFile(configFile)
	RegistryLoggerImplement("JLogger", CreateJLogger)
}

func RegistryFromConfig(tag string, config *Config) {
	cfg[tag] = *config
	RegistryLoggerImplement("JLogger", CreateJLogger)
}
