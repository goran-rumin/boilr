package template

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"text/template"

	"github.com/goran-rumin/boilr/pkg/boilr"
	"github.com/goran-rumin/boilr/pkg/prompt"
	"github.com/goran-rumin/boilr/pkg/util/osutil"
	"github.com/goran-rumin/boilr/pkg/util/stringutil"
	"github.com/goran-rumin/boilr/pkg/util/tlog"
)

// Interface is contains the behavior of boilr templates.
type Interface interface {
	// Executes the template on the given target directory path.
	Execute(string) error

	// If used, the template will execute using default values.
	UseDefaultValues()

	// Returns the metadata of the template.
	Info() Metadata
}

func (t dirTemplate) Info() Metadata {
	return t.Metadata
}

// Get retrieves the template from a path.
func Get(path string) (Interface, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	// TODO make context optional
	ctxt, err := func(fname string) (map[string]interface{}, error) {
		f, err := os.Open(fname)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}

			return nil, err
		}
		defer f.Close()

		buf, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}

		var metadata map[string]interface{}
		if err := json.Unmarshal(buf, &metadata); err != nil {
			return nil, err
		}

		return metadata, nil
	}(filepath.Join(absPath, boilr.ContextFileName))
	if err != nil {
		return nil, err
	}

	metadataExists, err := osutil.FileExists(filepath.Join(absPath, boilr.TemplateMetadataName))
	if err != nil {
		return nil, err
	}

	md, err := func() (Metadata, error) {
		if !metadataExists {
			return Metadata{}, nil
		}

		b, err := ioutil.ReadFile(filepath.Join(absPath, boilr.TemplateMetadataName))
		if err != nil {
			return Metadata{}, err
		}

		var m Metadata
		if err := json.Unmarshal(b, &m); err != nil {
			return Metadata{}, err
		}

		return m, nil
	}()
	if err != nil {
		return nil, err
	}

	tmpl := &dirTemplate{
		Context:  ctxt,
		FuncMap:  FuncMap,
		Path:     filepath.Join(absPath, boilr.TemplateDirName),
		Metadata: md,
	}
	if err = tmpl.validate(); err != nil {
		return nil, err
	}
	return tmpl, nil
}

type dirTemplate struct {
	Path     string
	Context  map[string]interface{}
	FuncMap  template.FuncMap
	Metadata Metadata

	alignment         string
	ShouldUseDefaults bool
}

func (t *dirTemplate) UseDefaultValues() {
	t.ShouldUseDefaults = true
}

func (t *dirTemplate) BindPrompts() {
	for templateVariable, defaultValue := range t.Context {
		if m, ok := defaultValue.(map[string]interface{}); ok {
			advancedMode := prompt.New(templateVariable, false)
			for childVariable, childDefaultValue := range m {
				t.bindPrompt(childVariable, childDefaultValue, &advancedMode)
			}
			continue
		}
		t.bindPrompt(templateVariable, defaultValue, nil)
	}
}

func (t *dirTemplate) bindPrompt(templateVariable string, defaultValue interface{}, parentPrompt *func() interface{}) {
	if t.ShouldUseDefaults {
		t.FuncMap[templateVariable] = func() interface{} {
			switch value := defaultValue.(type) {
			// First is the default value if it's a slice
			case []interface{}:
				return value[0]
			}
			return defaultValue
		}
	} else {
		prompt := prompt.New(templateVariable, defaultValue)

		if parentPrompt != nil {
			t.FuncMap[templateVariable] = func() interface{} {
				if val := (*parentPrompt)().(bool); val {
					return prompt()
				}
				return defaultValue
			}
		} else {
			t.FuncMap[templateVariable] = prompt
		}
	}
}

// Execute fills the template with the project metadata.
func (t *dirTemplate) Execute(dirPrefix string) error {
	t.BindPrompts()

	isOnlyWhitespace := func(buf []byte) bool {
		wsre := regexp.MustCompile(`\S`)

		return !wsre.Match(buf)
	}

	// TODO create io.ReadWriter from string
	// TODO refactor name manipulation
	return filepath.Walk(t.Path, func(filename string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Path relative to the root of the template directory
		oldName, err := filepath.Rel(t.Path, filename)
		if err != nil {
			return err
		}

		buf := stringutil.NewString("")

		// TODO translate errors into meaningful ones
		fnameTmpl := template.Must(template.
			New("file name template").
			Option(Options...).
			Funcs(FuncMap).
			Parse(oldName))

		if err := fnameTmpl.Execute(buf, nil); err != nil {
			return err
		}

		newName := buf.String()

		target := filepath.Join(dirPrefix, newName)

		if info.IsDir() {
			if err := os.Mkdir(target, 0755); err != nil {
				if !os.IsExist(err) {
					return err
				}
			}
		} else {
			fi, err := os.Lstat(filename)
			if err != nil {
				return err
			}

			// Delete target file if it exists
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, fi.Mode())
			if err != nil {
				return err
			}
			defer f.Close()

			defer func(fname string) {
				contents, err := ioutil.ReadFile(fname)
				if err != nil {
					tlog.Debug(fmt.Sprintf("couldn't read the contents of file %q, got error %q", fname, err))
					return
				}

				if isOnlyWhitespace(contents) {
					os.Remove(fname)
					return
				}
			}(f.Name())

			contentsTmpl := template.Must(template.
				New("file contents template").
				Option(Options...).
				Funcs(FuncMap).
				ParseFiles(filename))

			fileTemplateName := filepath.Base(filename)

			if err := contentsTmpl.ExecuteTemplate(f, fileTemplateName, nil); err != nil {
				return err
			}

			if !t.ShouldUseDefaults {
				tlog.Success(fmt.Sprintf("Created %s", newName))
			}
		}

		return nil
	})
}

// validate checks template for errors
func (t *dirTemplate) validate() error {
	t.BindPrompts()

	return filepath.Walk(t.Path, func(filename string, info os.FileInfo, err error) error {
		oldName, err := filepath.Rel(t.Path, filename)
		if err != nil {
			return err
		}

		_, err = template.New("file name template").Option(Options...).Funcs(FuncMap).Parse(oldName)
		if err != nil {
			return err
		}

		if !info.IsDir() {
			_, err = template.New("file contents template").Option(Options...).Funcs(FuncMap).ParseFiles(filename)
		}
		return err
	})
}
