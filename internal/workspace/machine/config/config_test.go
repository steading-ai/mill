package config_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/mill/internal/workspace/machine/config"
)

// checkout writes the named files into a fresh checkout root.
func checkout(files map[string]string) string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	for name, body := range files {
		Expect(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)).To(Succeed())
	}
	return dir
}

const goAndNode = "[tools]\ngo = \"1.25.7\"\nnode = { version = \"24.13.0\" }\n"

// names is the lineage names of caches in order.
func names(caches []config.Cache) []string {
	out := make([]string, 0, len(caches))
	for _, c := range caches {
		out = append(out, c.Name)
	}
	return out
}

// byName finds a cache by lineage name.
func byName(caches []config.Cache, name string) config.Cache {
	GinkgoHelper()
	for _, c := range caches {
		if c.Name == name {
			return c
		}
	}
	Fail("no lineage " + name)
	return config.Cache{}
}

var _ = Describe("mill.toml", Label("unit"), func() {
	Describe("Ecosystem defaults follow mise.toml", func() {
		It("declares the go and node lineages and no others", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{"mise.toml": goAndNode}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())

			Expect(names(cfg.Caches)).To(Equal([]string{"mise", "go-mod", "go-build", "npm-cache", "node_modules"}))
			Expect(byName(cfg.Caches, "mise")).To(Equal(config.Cache{Name: "mise", Path: "/home/agent/.local/share/mise", Scope: config.Global, Enabled: true}))
			Expect(byName(cfg.Caches, "go-mod")).To(Equal(config.Cache{Name: "go-mod", Path: "/home/agent/go/pkg/mod", Scope: config.Global, Enabled: true}))
			Expect(byName(cfg.Caches, "go-build")).To(Equal(config.Cache{Name: "go-build", Path: "/home/agent/.cache/go-build", Scope: config.Repo, Enabled: true}))
			Expect(byName(cfg.Caches, "npm-cache")).To(Equal(config.Cache{Name: "npm-cache", Path: "/home/agent/.npm", Scope: config.Global, Enabled: true}))
			Expect(byName(cfg.Caches, "node_modules")).To(Equal(config.Cache{Name: "node_modules", Path: "/work/node_modules", Scope: config.Repo, Enabled: true}))

			Expect(names(cfg.Caches)).NotTo(ContainElements("cargo-registry", "target", "gem-cache", "bundle", "bun-cache"))
			Expect(cfg.Image.Packages).To(BeEmpty())
			Expect(cfg.Machine.Nested).To(BeFalse())
		})

		It("reads tools declared as strings, lists, tables and core: names", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{"mise.toml": `
[tools]
"core:ruby" = "3.4"
rust = ["1.80", "1.81"]
bun = { version = "1.2" }
`}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(names(cfg.Caches)).To(Equal([]string{"mise", "cargo-registry", "target", "bun-cache", "node_modules", "gem-cache", "bundle"}))
		})

		It("keeps only the mise lineage when there is no mise.toml", func() {
			cfg, findings, err := config.Load(checkout(nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(names(cfg.Caches)).To(Equal([]string{"mise"}))
		})

		It("reports an invalid mise.toml as a finding and derives nothing from it", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{"mise.toml": "[tools\n"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(1))
			Expect(findings[0].File).To(Equal("mise.toml"))
			Expect(names(cfg.Caches)).To(Equal([]string{"mise"}))
		})
	})

	Describe("A path outside the session's home or /work is a finding", func() {
		DescribeTable("each row of the path rules is a finding naming the path",
			func(declared string) {
				cfg, findings, err := config.Load(checkout(map[string]string{
					"mise.toml": goAndNode,
					"mill.toml": "[cache.x]\npath = " + strconv(declared) + "\n",
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(findings).To(HaveLen(1))
				Expect(findings[0].File).To(Equal("mill.toml"))
				Expect(findings[0].Key).To(Equal("cache.x.path"))
				Expect(findings[0].Message).To(ContainSubstring(declared))
				Expect(findings[0].String()).To(HavePrefix("mill.toml: cache.x.path: "))
				// The run's configuration is the defaults.
				defaults, _, _ := config.Load(checkout(map[string]string{"mise.toml": goAndNode}))
				Expect(cfg).To(Equal(defaults))
			},
			Entry("/etc", "/etc"),
			Entry("/var/lib", "/var/lib"),
			Entry("/work", "/work"),
			Entry("/home/agent", "/home/agent"),
			Entry("~", "~"),
			Entry("/run/mill", "/run/mill"),
			Entry("../x", "../x"),
			Entry("a .. escape from /work", "/work/../etc"),
		)

		DescribeTable("a path under the home or the checkout is accepted",
			func(declared, resolved string) {
				cfg, findings, err := config.Load(checkout(map[string]string{
					"mill.toml": "[cache.x]\npath = " + strconv(declared) + "\n",
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(findings).To(BeEmpty())
				Expect(byName(cfg.Caches, "x")).To(Equal(config.Cache{Name: "x", Path: resolved, Scope: config.Repo, Enabled: true}))
			},
			Entry("~/.cache/x", "~/.cache/x", "/home/agent/.cache/x"),
			Entry("/work/x", "/work/x", "/work/x"),
		)
	})

	Describe("A declared cache overrides a default of the same name", func() {
		It("replaces the default's path and keeps its scope", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mise.toml": goAndNode,
				"mill.toml": "[cache.go-build]\npath = \"/work/.gocache\"\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(byName(cfg.Caches, "go-build")).To(Equal(config.Cache{Name: "go-build", Path: "/work/.gocache", Scope: config.Repo, Enabled: true}))
			Expect(names(cfg.Caches)).To(Equal([]string{"mise", "go-mod", "go-build", "npm-cache", "node_modules"}))
		})

		It("removes a default with enabled = false", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mise.toml": goAndNode,
				"mill.toml": "[cache.go-mod]\nenabled = false\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(names(cfg.Caches)).To(Equal([]string{"mise", "go-build", "npm-cache", "node_modules"}))
		})

		It("keeps a default's path when only its key is declared", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mise.toml": goAndNode,
				"mill.toml": "[cache.go-build]\nkey = [\"go.mod\", \"./go.sum\"]\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(byName(cfg.Caches, "go-build")).To(Equal(config.Cache{Name: "go-build", Path: "/home/agent/.cache/go-build", Scope: config.Repo, Key: []string{"go.mod", "go.sum"}, Enabled: true}))
		})

		It("is a finding to declare a new cache without a path, or with a key outside the checkout", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[cache.a]\nkey = [\"x\"]\n[cache.b]\npath = \"/work/b\"\nkey = [\"../x\", \"/etc/passwd\"]\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(3))
			Expect(findings[0].Key).To(Equal("cache.a.path"))
			Expect(findings[1].Key).To(Equal("cache.b.key[0]"))
			Expect(findings[2].Key).To(Equal("cache.b.key[1]"))
			Expect(names(cfg.Caches)).To(Equal([]string{"mise"}))
		})

		It("is a finding to name a lineage with a path separator in it", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[cache.\"../x\"]\npath = \"/work/x\"\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(1))
			Expect(findings[0].Key).To(Equal("cache.../x"))
			Expect(names(cfg.Caches)).To(Equal([]string{"mise"}))
		})
	})

	Describe("Packages", func() {
		It("are the same sorted, deduplicated list whatever their order and repeats", func() {
			one, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[image]\npackages = [\"vips-devel\", \"libpq-devel\"]\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			two, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[image]\npackages = [\"libpq-devel\", \"vips-devel\", \"libpq-devel\"]\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
			Expect(one.Image.Packages).To(Equal([]string{"libpq-devel", "vips-devel"}))
			Expect(two).To(Equal(one))
		})

		It("drops a name that is not a package name with a finding", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[image]\npackages = [\"ok\", \"no spaces\", \"$(rm)\"]\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(2))
			Expect(findings[0].Key).To(Equal("image.packages[1]"))
			Expect(findings[1].Key).To(Equal("image.packages[2]"))
			Expect(cfg.Image.Packages).To(Equal([]string{"ok"}))
		})
	})

	Describe("A validation error does not fail the run", func() {
		It("returns defaults plus one finding with the parse error for invalid TOML", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mise.toml": goAndNode,
				"mill.toml": "[image\npackages = [",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(1))
			Expect(findings[0].File).To(Equal("mill.toml"))
			Expect(findings[0].Key).To(BeEmpty())
			Expect(findings[0].Message).To(ContainSubstring("not valid TOML"))
			Expect(findings[0].Message).To(ContainSubstring("defaults"))
			defaults, _, _ := config.Load(checkout(map[string]string{"mise.toml": goAndNode}))
			Expect(cfg).To(Equal(defaults))
		})

		It("reports an unknown table as a finding and reads the rest", func() {
			cfg, findings, err := config.Load(checkout(map[string]string{
				"mill.toml": "[network]\negress = \"open\"\n[machine]\nnested = true\n[cache.x]\npath = \"/work/x\"\ncolour = \"blue\"\n",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(HaveLen(2))
			Expect(findings[0].Key).To(Equal("network"))
			Expect(findings[1].Key).To(Equal("cache.x.colour"))
			Expect(cfg.Machine.Nested).To(BeTrue())
			Expect(names(cfg.Caches)).To(Equal([]string{"mise", "x"}))
		})

		It("returns an error only when a file cannot be read", func() {
			dir := checkout(nil)
			Expect(os.Mkdir(filepath.Join(dir, "mill.toml"), 0o755)).To(Succeed())
			_, _, err := config.Load(dir)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("A repo cannot reach a host path through its declaration", func() {
		It("binds every host side under the run directory, /run/mill, or the worker's skill directory", func() {
			cfg, _, err := config.Load(checkout(map[string]string{
				"mise.toml": goAndNode,
				"mill.toml": `
[cache.x]
path = "/work/x"
source = "/etc"
host = "/var/lib/mill"

[cache.go-build]
path = "~/.cache/gb"

[cache."etc"]
path = "/etc"
`,
			}))
			Expect(err).NotTo(HaveOccurred())

			mounts := config.Mounts{
				Run:    "/var/lib/mill/runs/01RUN",
				Socket: "/run/mill/01RUN",
				Skill:  "/opt/mill/skill",
			}
			binds := config.Binds(cfg, mounts)
			Expect(binds).NotTo(BeEmpty())
			for _, bind := range binds {
				Expect(bind.Host).To(SatisfyAny(
					HavePrefix(mounts.Run+"/"),
					Equal(mounts.Socket),
					Equal(mounts.Skill),
				), "host side %q of %q", bind.Host, bind.Machine)
				Expect(bind.Host).NotTo(ContainSubstring(".."))
			}
			Expect(binds).To(ContainElements(
				config.Bind{Host: "/var/lib/mill/runs/01RUN/workspace", Machine: "/work"},
				config.Bind{Host: "/run/mill/01RUN", Machine: "/run/mill"},
				config.Bind{Host: "/opt/mill/skill", Machine: "/home/agent/.claude/skills/mill", ReadOnly: true},
				config.Bind{Host: "/var/lib/mill/runs/01RUN/cache/x", Machine: "/work/x"},
				config.Bind{Host: "/var/lib/mill/runs/01RUN/cache/go-build", Machine: "/home/agent/.cache/gb"},
			))
			for _, bind := range binds {
				Expect(bind.Machine).NotTo(Equal("/etc"))
			}

			cacheBinds := config.CacheBinds(cfg)
			Expect(cacheBinds).To(HaveLen(len(cfg.Caches)))
			Expect(cacheBinds[0].Name).To(Equal("mise"))
			Expect(cacheBinds[0].Global).To(BeTrue())
		})
	})
})

// strconv quotes a value as a TOML basic string.
func strconv(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

var _ = Describe("The grammar's keys", Label("unit"), func() {
	It("are every key Load accepts, read off the grammar itself", func() {
		Expect(config.Keys()).To(Equal([]string{
			"image.packages",
			"cache.<name>.path",
			"cache.<name>.key",
			"cache.<name>.enabled",
			"machine.nested",
		}))
	})
})
