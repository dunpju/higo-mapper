// higo-mapper：连库反向生成 entity struct 与 mapper XML 骨架。
//
// 用法：
//
//	# MySQL 存量库 → 生成到 src/models/ 与 mappers/
//	higo-mapper generate --url mysql://user:pass@localhost:3306/mydb \
//	    --out-dir src/models --xml-dir mappers --namespace app.dao
//
//	# 只生成指定表；--dry-run 只打印不写盘
//	higo-mapper generate --url sqlite:app.db --tables users,orders --dry-run
//
// 产物：
//   - {out_dir}/{table}.go：每表一个 struct（json tag 映射列名 + 可空列指针类型）
//   - {xml_dir}/{Table}Dao.xml：resultMap（含 json/decimal 列的 goType）+
//     findById/findAll/insert/insertBatch/updateById/deleteById 骨架
//
// 说明：方言由 URL scheme 推断（mysql/mariadb → MySQL；sqlite → SQLite；
// postgres 暂不支持内省）；生成物是起点而非终稿，按业务调整命名与语句。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		if err := runGenerate(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "[higo-mapper] 错误: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "[higo-mapper] 未知命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `higo-mapper — 连库反向生成 entity 与 mapper XML

用法:
  higo-mapper generate [flags]

Flags:
  --url string        数据库连接 URL（方言按 scheme 推断：mysql://… | mariadb://… | sqlite:…）
  --tables strings    仅生成这些表（逗号分隔）；缺省生成全部用户表
  --out-dir dir       entity .go 输出目录（默认 ./generated）
  --xml-dir dir       mapper XML 输出目录（默认 ./generated/mappers）
  --pkg name          entity 包名（默认 models）
  --namespace string  XML namespace 前缀（默认 app）
  --dry-run           只打印将生成的文件，不写盘
`)
}

// generateOptions generate 子命令参数
type generateOptions struct {
	url       string
	tables    []string
	outDir    string
	xmlDir    string
	pkg       string
	namespace string
	dryRun    bool
}

func parseGenerateArgs(args []string) (*generateOptions, error) {
	opt := &generateOptions{
		outDir:    "./generated",
		xmlDir:    "./generated/mappers",
		pkg:       "models",
		namespace: "app",
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s 缺少取值", a)
			}
			return args[i], nil
		}
		var err error
		switch a {
		case "--url":
			if opt.url, err = next(); err != nil {
				return nil, err
			}
		case "--tables":
			v, err := next()
			if err != nil {
				return nil, err
			}
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					opt.tables = append(opt.tables, t)
				}
			}
		case "--out-dir":
			if opt.outDir, err = next(); err != nil {
				return nil, err
			}
		case "--xml-dir":
			if opt.xmlDir, err = next(); err != nil {
				return nil, err
			}
		case "--pkg":
			if opt.pkg, err = next(); err != nil {
				return nil, err
			}
		case "--namespace":
			if opt.namespace, err = next(); err != nil {
				return nil, err
			}
		case "--dry-run":
			opt.dryRun = true
		default:
			return nil, fmt.Errorf("未知参数 %q（见 --help）", a)
		}
	}
	if opt.url == "" {
		return nil, fmt.Errorf("--url 为必填参数")
	}
	return opt, nil
}

func runGenerate(args []string) error {
	opt, err := parseGenerateArgs(args)
	if err != nil {
		return err
	}
	dialect, ok := dialectOfURL(opt.url)
	if !ok {
		return fmt.Errorf("无法从 URL 推断方言（支持 mysql:// 与 sqlite:）: %s", opt.url)
	}
	db, err := connectDB(opt.url)
	if err != nil {
		return err
	}
	defer db.Close()

	tableNames := opt.tables
	if len(tableNames) == 0 {
		if tableNames, err = listTables(db, dialect); err != nil {
			return err
		}
	}
	if len(tableNames) == 0 {
		fmt.Fprintln(os.Stderr, "[higo-mapper] 未发现任何表，无产物。")
		return nil
	}

	if !opt.dryRun {
		if err := os.MkdirAll(opt.outDir, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(opt.xmlDir, 0o755); err != nil {
			return err
		}
	}

	generated := 0
	for _, table := range tableNames {
		meta, err := introspectTable(db, dialect, table)
		if err != nil {
			return err
		}
		entity := generateEntity(meta, dialect, opt.pkg)
		xml := generateXML(meta, dialect, opt.namespace)

		structName := pascalCase(table)
		goPath := filepath.Join(opt.outDir, table+".go")
		xmlPath := filepath.Join(opt.xmlDir, structName+"Dao.xml")

		if opt.dryRun {
			fmt.Printf("── %s ──\n%s\n── %s ──\n%s\n", filepath.ToSlash(goPath), entity,
				filepath.ToSlash(xmlPath), xml)
			continue
		}
		if err := os.WriteFile(goPath, []byte(entity), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
			return err
		}
		generated++
		fmt.Printf("生成 %s / %s\n", filepath.ToSlash(goPath), filepath.ToSlash(xmlPath))
	}

	fmt.Printf("[higo-mapper] 完成：%d 个表（%d entity / %d XML）\n",
		len(tableNames), generated, generated)
	return nil
}

// connectDB 由 URL 打开连接池（方言经 scheme 推断，DSN 转换复用运行时逻辑）
func connectDB(rawURL string) (*sql.DB, error) {
	dialect, _ := dialectOfURL(rawURL)
	dsn, err := resolveCLIDSN(dialect, rawURL)
	if err != nil {
		return nil, err
	}
	driver := driverCLIDriver(dialect)
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("无法连接数据库 '%s': %w", rawURL, err)
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("无法连接数据库 '%s': %w", rawURL, err)
	}
	return db, nil
}
