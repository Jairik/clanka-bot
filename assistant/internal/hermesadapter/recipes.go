package hermesadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Jairik/clanka-bot/assistant/internal/tool"
	"github.com/google/jsonschema-go/jsonschema"
)

const (
	recipePrefix   = "recipe."
	recipesKey     = "hermes.tool_recipes"
	maxRecipes     = 64
	maxRecipeArgs  = 8 << 10
	maxRecipeName  = 56 // Leaves room for the recipe__ prefix in a 64-character function name.
	maxDescription = 500
)

var (
	ErrInvalidRecipe      = errors.New("invalid recipe")
	ErrRecipeNotFound     = errors.New("recipe not found")
	ErrRecipesUnavailable = errors.New("recipe manager is unavailable")
)

// Recipe binds optional arguments to a vetted Go tool.
type Recipe struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	BaseTool    string                     `json:"base_tool"`
	Arguments   map[string]json.RawMessage `json:"arguments"`
	Enabled     bool                       `json:"enabled"`
}

// RecipeTools is the authenticated Dashboard view.
type RecipeTools struct {
	Available []RecipeBase `json:"available"`
	Recipes   []Recipe     `json:"recipes"`
}

// RecipeBase describes a vetted base without loading its schema.
type RecipeBase struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// RecipeTools returns saved recipes and the base tools currently eligible for new recipes.
func (a *Adapter) RecipeTools(ctx context.Context) (RecipeTools, error) {
	if a == nil || a.registry == nil || a.settings == nil {
		return RecipeTools{}, ErrRecipesUnavailable
	}
	recipes, err := a.readRecipes(ctx)
	if err != nil {
		return RecipeTools{}, err
	}
	return a.recipeTools(recipes), nil
}

// SaveRecipe creates or replaces one recipe and returns the committed view.
func (a *Adapter) SaveRecipe(ctx context.Context, recipe Recipe) (RecipeTools, error) {
	if a == nil || a.registry == nil || a.settings == nil {
		return RecipeTools{}, ErrRecipesUnavailable
	}
	if !validRecipeName(recipe.Name) {
		return RecipeTools{}, fmt.Errorf("%w: use a lowercase name of up to %d characters, starting with a letter, with digits or single underscores", ErrInvalidRecipe, maxRecipeName)
	}
	if _, exists := a.registry.Info(recipePrefix + recipe.Name); exists {
		return RecipeTools{}, fmt.Errorf("%w: that name is reserved", ErrInvalidRecipe)
	}
	recipe.Description = strings.TrimSpace(recipe.Description)
	if recipe.Description == "" || utf8.RuneCountInString(recipe.Description) > maxDescription || strings.ContainsFunc(recipe.Description, unicode.IsControl) {
		return RecipeTools{}, fmt.Errorf("%w: provide a single-line description of up to %d characters", ErrInvalidRecipe, maxDescription)
	}
	if recipe.Arguments == nil {
		recipe.Arguments = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(recipe)
	if err != nil || len(raw) > maxBodyBytes || a.rejectSecrets(raw) != nil {
		return RecipeTools{}, fmt.Errorf("%w: provide valid JSON without credentials", ErrInvalidRecipe)
	}
	arguments, err := json.Marshal(recipe.Arguments)
	if err != nil || len(arguments) > maxRecipeArgs {
		return RecipeTools{}, fmt.Errorf("%w: fixed arguments must fit in %d bytes", ErrInvalidRecipe, maxRecipeArgs)
	}

	a.recipeMu.Lock()
	defer a.recipeMu.Unlock()
	recipes, err := a.readRecipes(ctx)
	if err != nil {
		return RecipeTools{}, err
	}
	index := slices.IndexFunc(recipes, func(saved Recipe) bool { return saved.Name == recipe.Name })
	info, denied := a.inspectBase(recipe.BaseTool)
	// An existing recipe can be turned off after its base loses eligibility.
	disableOnly := !recipe.Enabled && index >= 0 && sameRecipeConfig(recipes[index], recipe)
	if !disableOnly {
		if denied != nil || !Allowlisted(recipe.BaseTool) {
			return RecipeTools{}, fmt.Errorf("%w: choose an available read-only base tool", ErrInvalidRecipe)
		}
		if _, err := recipeInputSchema(info, recipe.Arguments); err != nil {
			return RecipeTools{}, fmt.Errorf("%w: fixed arguments must match the base tool's input schema", ErrInvalidRecipe)
		}
	}
	if index >= 0 {
		recipes[index] = recipe
	} else {
		if len(recipes) >= maxRecipes {
			return RecipeTools{}, fmt.Errorf("%w: at most %d recipes can be saved", ErrInvalidRecipe, maxRecipes)
		}
		recipes = append(recipes, recipe)
		slices.SortFunc(recipes, func(a, b Recipe) int { return strings.Compare(a.Name, b.Name) })
	}
	return a.writeRecipes(ctx, recipes)
}

// DeleteRecipe removes a recipe even if its base tool is gone.
func (a *Adapter) DeleteRecipe(ctx context.Context, name string) (RecipeTools, error) {
	if a == nil || a.settings == nil {
		return RecipeTools{}, ErrRecipesUnavailable
	}
	a.recipeMu.Lock()
	defer a.recipeMu.Unlock()
	recipes, err := a.readRecipes(ctx)
	if err != nil {
		return RecipeTools{}, err
	}
	index := slices.IndexFunc(recipes, func(saved Recipe) bool { return saved.Name == name })
	if index < 0 {
		return RecipeTools{}, ErrRecipeNotFound
	}
	return a.writeRecipes(ctx, slices.Delete(recipes, index, index+1))
}

// readRecipes uses the private SQLite settings API so the generic Settings API cannot write recipes.
func (a *Adapter) readRecipes(ctx context.Context) ([]Recipe, error) {
	recipes := []Recipe{}
	if a == nil || a.settings == nil {
		return recipes, nil
	}
	raw, found, err := a.settings.GetPrivate(ctx, recipesKey)
	if err != nil || !found {
		return recipes, err
	}
	if err := json.Unmarshal([]byte(raw), &recipes); err != nil || recipes == nil {
		return nil, errors.New("failed to read saved Hermes recipes")
	}
	return recipes, nil
}

// writeRecipes replaces the stored snapshot in a single SQLite write.
func (a *Adapter) writeRecipes(ctx context.Context, recipes []Recipe) (RecipeTools, error) {
	raw, err := json.Marshal(recipes)
	if err != nil {
		return RecipeTools{}, err
	}
	if err := a.settings.SetPrivate(ctx, recipesKey, string(raw)); err != nil {
		return RecipeTools{}, err
	}
	return a.recipeTools(recipes), nil
}

// recipeTools includes eligible bases even when the owner has yet to grant account access.
func (a *Adapter) recipeTools(recipes []Recipe) RecipeTools {
	view := RecipeTools{Available: []RecipeBase{}, Recipes: recipes}
	for _, name := range a.order {
		if info, err := a.inspectBase(name); err == nil && Allowlisted(name) {
			view.Available = append(view.Available, RecipeBase{Name: name, Description: info.Description})
		}
	}
	return view
}

// resolve rechecks both the recipe's availability and the target's live policy and grant.
func (a *Adapter) resolve(ctx context.Context, name string) (tool.Info, *Recipe, error) {
	slug, isRecipe := strings.CutPrefix(name, recipePrefix)
	if !isRecipe {
		info, err := a.inspect(name)
		return info, nil, err
	}
	recipes, err := a.readRecipes(ctx)
	if err != nil {
		return tool.Info{}, nil, deny(name, causeUnknown)
	}
	for _, recipe := range recipes {
		if recipe.Name == slug && a.recipeDiscoverable(name, recipe) {
			info, err := a.inspect(recipe.BaseTool)
			return info, &recipe, err
		}
	}
	return tool.Info{}, nil, deny(name, causeUnknown)
}

// recipeDiscoverable checks recipe metadata without opening any schemas.
func (a *Adapter) recipeDiscoverable(name string, recipe Recipe) bool {
	if a.registry == nil || !recipe.Enabled || !validRecipeName(recipe.Name) || !Allowlisted(recipe.BaseTool) {
		return false
	}
	if _, exists := a.registry.Info(name); exists {
		return false
	}
	raw, err := json.Marshal(recipe)
	return err == nil && a.rejectSecrets(raw) == nil
}

// validRecipeName uses the same identifier rules as the runtime's function names.
func validRecipeName(name string) bool {
	_, err := tool.WireName(recipePrefix + name)
	return len(name) <= maxRecipeName && err == nil
}

// sameRecipeConfig permits disabling an unchanged recipe whose base was retired.
func sameRecipeConfig(saved, update Recipe) bool {
	saved.Enabled, update.Enabled = false, false
	left, _ := json.Marshal(saved)
	right, _ := json.Marshal(update)
	return string(left) == string(right)
}

// recipeInputSchema validates partial fixed arguments and removes them from the callable contract.
func recipeInputSchema(info tool.Info, fixed map[string]json.RawMessage) (json.RawMessage, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(info.InputSchema, &schema); err != nil {
		return nil, err
	}
	partial := schema
	partial.Required = nil
	resolved, err := partial.Resolve(nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(fixed)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if len(fixed) != 0 {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
	}
	if err := resolved.Validate(values); err != nil {
		return nil, err
	}
	for key, value := range fixed {
		// JSON Schema accepts 1.0 and large numbers that Go's int fields reject.
		if property := schema.Properties[key]; property != nil && property.Type == "integer" {
			if err := json.Unmarshal(value, new(int)); err != nil {
				return nil, err
			}
		}
		delete(schema.Properties, key)
	}
	schema.Required = slices.DeleteFunc(schema.Required, func(key string) bool {
		_, bound := fixed[key]
		return bound
	})
	return json.Marshal(&schema)
}

// mergeRecipeArguments rejects attempts to supply an argument owned by the recipe.
func mergeRecipeArguments(raw json.RawMessage, fixed map[string]json.RawMessage) (json.RawMessage, error) {
	args := make(map[string]json.RawMessage, len(fixed))
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &args); err != nil || args == nil {
			return nil, ErrInvalidRecipe
		}
	}
	for key, value := range fixed {
		if _, exists := args[key]; exists {
			return nil, ErrInvalidRecipe
		}
		args[key] = value
	}
	return json.Marshal(args)
}
