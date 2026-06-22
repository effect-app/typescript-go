package declarations

import (
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/debug"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/jsnum"
	"github.com/microsoft/typescript-go/internal/modulespecifiers"
	"github.com/microsoft/typescript-go/internal/nodebuilder"
	"github.com/microsoft/typescript-go/internal/printer"
	"github.com/microsoft/typescript-go/internal/scanner"
	"github.com/microsoft/typescript-go/internal/transformers"
	"github.com/microsoft/typescript-go/internal/tspath"
)

type ReferencedFilePair struct {
	file *ast.SourceFile
	ref  *ast.FileReference
}

type OutputPaths interface {
	DeclarationFilePath() string
	JsFilePath() string
}

// Used to be passed in the TransformationContext, which is now just an EmitContext
type DeclarationEmitHost interface {
	modulespecifiers.ModuleSpecifierGenerationHost
	GetCurrentDirectory() string
	UseCaseSensitiveFileNames() bool
	GetSourceFileFromReference(origin *ast.SourceFile, ref *ast.FileReference) *ast.SourceFile

	GetOutputPathsFor(file *ast.SourceFile, forceDtsPaths bool) OutputPaths
	GetResolutionModeOverride(node *ast.Node) core.ResolutionMode
	GetEffectiveDeclarationFlags(node *ast.Node, flags ast.ModifierFlags) ast.ModifierFlags
	GetEmitResolver() printer.EmitResolver
}

type thisPropertyAssignmentKey struct {
	name      string
	node      *ast.Node
	isStatic  bool
	isPrivate bool
}

func getThisPropertyAssignmentKey(name *ast.Node, node *ast.Node, isStatic bool) thisPropertyAssignmentKey {
	isPrivate := ast.IsPrivateIdentifier(name)
	if name != nil && !ast.IsDynamicName(name) {
		if nameText, ok := ast.TryGetTextOfPropertyName(name); ok {
			return thisPropertyAssignmentKey{name: nameText, isStatic: isStatic, isPrivate: isPrivate}
		}
	}
	return thisPropertyAssignmentKey{node: node, isStatic: isStatic, isPrivate: isPrivate}
}

type DeclarationTransformer struct {
	transformers.Transformer
	host                DeclarationEmitHost
	compilerOptions     *core.CompilerOptions
	tracker             *SymbolTrackerImpl
	state               *SymbolTrackerSharedState
	resolver            printer.EmitResolver
	declarationFilePath string
	declarationMapPath  string

	needsDeclare                     bool
	needsScopeFixMarker              bool
	resultHasScopeMarker             bool
	enclosingDeclaration             *ast.Node
	resultHasExternalModuleIndicator bool
	suppressNewDiagnosticContexts    bool
	witnessedCjsExports              collections.Set[string]
	lateStatementReplacementMap      map[ast.NodeId]*ast.Node
	expandoHosts                     map[ast.NodeId]*ast.Node   // store the result of transforming expando hosts so they can be inserted later if the host is actually referenced
	expandoMembers                   map[ast.NodeId][]*ast.Node // store any found expando _members_ after transforming them so *if* the host is referenced, they can be emitted alongside it
	seenProperties                   collections.Set[thisPropertyAssignmentKey]
	thisPropertyAssignmentsCollected []*ast.Node
	rawReferencedFiles               []ReferencedFilePair
	rawTypeReferenceDirectives       []*ast.FileReference
	rawLibReferenceDirectives        []*ast.FileReference
	bindingNameVisitor               *ast.NodeVisitor
	expressionVisitor                *ast.NodeVisitor
	cjsExportAssignmentVisitor       *ast.NodeVisitor
	exportStrippingVisitor           *ast.NodeVisitor
	thisPropertyVisitor              *ast.NodeVisitor

	cjsExportAssignment          *ast.Node
	cjsExportMembers             []*ast.Node
	cjsExportAssignmentName      *ast.Node // tracks the name node used for `export =` in CJS module.exports assignments
	declareStrippingVisitor      *ast.NodeVisitor
	inClassExpressionDeclaration bool // true when serializing members of a class expression kept as a class declaration
}

// TODO: Convert to transformers.TransformerFactory signature to allow more automatic composition with other transforms
func NewDeclarationTransformer(host DeclarationEmitHost, context *printer.EmitContext, compilerOptions *core.CompilerOptions, declarationFilePath string, declarationMapPath string) *DeclarationTransformer {
	resolver := host.GetEmitResolver()
	state := &SymbolTrackerSharedState{isolatedDeclarations: compilerOptions.IsolatedDeclarations.IsTrue(), stripInternal: compilerOptions.StripInternal.IsTrue(), resolver: resolver}
	tracker := NewSymbolTracker(host, resolver, state)
	// TODO: Use new host GetOutputPathsFor method instead of passing in entrypoint paths (which will also better support bundled emit)
	tx := &DeclarationTransformer{
		host:                host,
		compilerOptions:     compilerOptions,
		tracker:             tracker,
		state:               state,
		resolver:            resolver,
		declarationFilePath: declarationFilePath,
		declarationMapPath:  declarationMapPath,
	}
	tx.state.reportExpandoFunctionErrors = func(node *ast.Node) {
		if !tx.state.isolatedDeclarations {
			return
		}
		props := resolver.GetPropertiesOfContainerFunction(node)
		for _, p := range props {
			if ast.IsExpandoPropertyDeclaration(p.ValueDeclaration) {
				errorTarget := p.ValueDeclaration
				if ast.IsBinaryExpression(errorTarget) {
					errorTarget = errorTarget.AsBinaryExpression().Left
				}
				tx.state.addDiagnostic(createDiagnosticForNode(errorTarget, diagnostics.Assigning_properties_to_functions_without_declaring_them_is_not_supported_with_isolatedDeclarations_Add_an_explicit_declaration_for_the_properties_assigned_to_this_function))
			}
		}
	}
	tx.NewTransformer(tx.visit, context)
	tx.bindingNameVisitor = tx.EmitContext().NewNodeVisitor(tx.visitBindingName)
	tx.expressionVisitor = tx.EmitContext().NewNodeVisitor(tx.visitNestedExpression)
	tx.exportStrippingVisitor = tx.EmitContext().NewNodeVisitor(tx.stripExportModifiers)
	tx.thisPropertyVisitor = tx.EmitContext().NewNodeVisitor(tx.visitThisPropertyAssignments)
	tx.cjsExportAssignmentVisitor = tx.EmitContext().NewNodeVisitor(tx.visitCJSExportAssignments)
	tx.declareStrippingVisitor = tx.EmitContext().NewNodeVisitor(tx.stripDeclareModifiers)
	return tx
}

func (tx *DeclarationTransformer) GetDiagnostics() []*ast.Diagnostic {
	return tx.state.diagnostics
}

func (tx *DeclarationTransformer) shouldStripInternal(node *ast.Node) bool {
	return tx.state.stripInternal && node != nil && tx.isInternalDeclaration(node, tx.state.currentSourceFile)
}

func (tx *DeclarationTransformer) isInternalDeclaration(node *ast.Node, sourceFile *ast.SourceFile) bool {
	if node == nil {
		return false
	}
	parseTreeNode := tx.EmitContext().MostOriginal(node)
	if !ast.IsParseTreeNode(parseTreeNode) {
		return false
	}
	if parseTreeNode.Kind == ast.KindParameter {
		params := parseTreeNode.Parent.Parameters()
		paramIdx := slices.IndexFunc(params, func(p *ast.ParameterDeclarationNode) bool {
			return p.AsNode() == parseTreeNode
		})
		var previousSibling *ast.Node
		if paramIdx > 0 {
			previousSibling = params[paramIdx-1].AsNode()
		}

		text := sourceFile.Text()
		var commentRanges []ast.CommentRange

		if previousSibling != nil {
			// to handle
			// ... parameters, /** @internal */
			// public param: string
			trailingPos := scanner.SkipTriviaEx(text, previousSibling.End()+1, &scanner.SkipTriviaOptions{StopAtComments: true})
			for comment := range scanner.GetTrailingCommentRanges(tx.Factory().AsNodeFactory(), text, trailingPos) {
				commentRanges = append(commentRanges, comment)
			}
			for comment := range scanner.GetLeadingCommentRanges(tx.Factory().AsNodeFactory(), text, node.Pos()) {
				commentRanges = append(commentRanges, comment)
			}
		} else {
			trailingPos := scanner.SkipTriviaEx(text, node.Pos(), &scanner.SkipTriviaOptions{StopAtComments: true})
			for comment := range scanner.GetTrailingCommentRanges(tx.Factory().AsNodeFactory(), text, trailingPos) {
				commentRanges = append(commentRanges, comment)
			}
		}

		if len(commentRanges) > 0 {
			return hasInternalAnnotation(commentRanges[len(commentRanges)-1], sourceFile)
		}
		return false
	}

	for commentRange := range tx.getLeadingCommentRangesOfNode(parseTreeNode, sourceFile) {
		if hasInternalAnnotation(commentRange, sourceFile) {
			return true
		}
	}
	return false
}

func (tx *DeclarationTransformer) getLeadingCommentRangesOfNode(node *ast.Node, sourceFile *ast.SourceFile) iter.Seq[ast.CommentRange] {
	if node == nil || node.Kind == ast.KindJsxText {
		return nil
	}
	return scanner.GetLeadingCommentRanges(tx.Factory().AsNodeFactory(), sourceFile.Text(), node.Pos())
}

func hasInternalAnnotation(commentRange ast.CommentRange, sourceFile *ast.SourceFile) bool {
	comment := sourceFile.Text()[commentRange.Pos():commentRange.End()]
	return strings.Contains(comment, "@internal")
}

const declarationEmitNodeBuilderFlags = nodebuilder.FlagsMultilineObjectLiterals |
	nodebuilder.FlagsWriteClassExpressionAsTypeLiteral |
	nodebuilder.FlagsUseTypeOfFunction |
	nodebuilder.FlagsUseStructuralFallback |
	nodebuilder.FlagsAllowEmptyTuple |
	nodebuilder.FlagsGenerateNamesForShadowedTypeParams |
	nodebuilder.FlagsNoTruncation

const declarationEmitInternalNodeBuilderFlags = nodebuilder.InternalFlagsAllowUnresolvedNames

// functions as both `visitDeclarationStatements` and `transformRoot`, utilitzing SyntaxList nodes
func (tx *DeclarationTransformer) visit(node *ast.Node) *ast.Node {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case ast.KindSourceFile:
		return tx.visitSourceFile(node.AsSourceFile())
	// statements we keep but do something to
	case ast.KindFunctionDeclaration,
		ast.KindModuleDeclaration,
		ast.KindImportEqualsDeclaration,
		ast.KindInterfaceDeclaration,
		ast.KindClassDeclaration,
		ast.KindJSTypeAliasDeclaration,
		ast.KindTypeAliasDeclaration,
		ast.KindEnumDeclaration,
		ast.KindVariableStatement,
		ast.KindImportDeclaration,
		ast.KindJSImportDeclaration,
		ast.KindExportDeclaration,
		ast.KindExportAssignment:
		return tx.visitDeclarationStatements(node)
	// statements we elide
	case ast.KindBreakStatement,
		ast.KindContinueStatement,
		ast.KindDebuggerStatement,
		ast.KindDoStatement,
		ast.KindEmptyStatement,
		ast.KindForInStatement,
		ast.KindForOfStatement,
		ast.KindForStatement,
		ast.KindIfStatement,
		ast.KindLabeledStatement,
		ast.KindReturnStatement,
		ast.KindSwitchStatement,
		ast.KindThrowStatement,
		ast.KindTryStatement,
		ast.KindWhileStatement,
		ast.KindWithStatement,
		ast.KindNotEmittedStatement,
		ast.KindBlock,
		ast.KindMissingDeclaration,
		ast.KindExpressionStatement:
		return nil
	// parts of things, things we just visit children of
	default:
		return tx.visitDeclarationSubtree(node)
	}
}

func throwDiagnostic(result printer.SymbolAccessibilityResult) *SymbolAccessibilityDiagnostic {
	panic("Diagnostic emitted without context")
}

func (tx *DeclarationTransformer) visitSourceFile(node *ast.SourceFile) *ast.Node {
	tx.cjsExportAssignmentName = nil
	if node.IsDeclarationFile {
		return node.AsNode()
	}

	tx.needsDeclare = true
	tx.needsScopeFixMarker = false
	tx.resultHasScopeMarker = false
	tx.enclosingDeclaration = node.AsNode()
	tx.state.getSymbolAccessibilityDiagnostic = throwDiagnostic
	tx.resultHasExternalModuleIndicator = false
	tx.suppressNewDiagnosticContexts = false
	tx.state.lateMarkedStatements = make([]*ast.Node, 0)
	tx.lateStatementReplacementMap = make(map[ast.NodeId]*ast.Node)
	tx.expandoHosts = make(map[ast.NodeId]*ast.Node)
	tx.expandoMembers = make(map[ast.NodeId][]*ast.Node)
	tx.rawReferencedFiles = make([]ReferencedFilePair, 0)
	tx.rawTypeReferenceDirectives = make([]*ast.FileReference, 0)
	tx.rawLibReferenceDirectives = make([]*ast.FileReference, 0)
	tx.witnessedCjsExports.Clear()
	tx.state.currentSourceFile = node
	tx.collectFileReferences(node)
	tx.resolver.PrecalculateDeclarationEmitVisibility(node)
	updated := tx.transformSourceFile(node)
	tx.state.currentSourceFile = nil
	return updated
}

func (tx *DeclarationTransformer) collectFileReferences(sourceFile *ast.SourceFile) {
	tx.rawReferencedFiles = append(tx.rawReferencedFiles, core.Map(sourceFile.ReferencedFiles, func(ref *ast.FileReference) ReferencedFilePair { return ReferencedFilePair{file: sourceFile, ref: ref} })...)
	tx.rawTypeReferenceDirectives = append(tx.rawTypeReferenceDirectives, sourceFile.TypeReferenceDirectives...)
	tx.rawLibReferenceDirectives = append(tx.rawLibReferenceDirectives, sourceFile.LibReferenceDirectives...)
}

func nodeOrSyntaxListChildren(node *ast.Node) []*ast.Node {
	if ast.IsSyntaxList(node) {
		return node.AsSyntaxList().Children
	}
	return []*ast.Node{node}
}

func flattenSyntaxLists(nodes []*ast.Node) []*ast.Node {
	return core.FlatMap(nodes, nodeOrSyntaxListChildren)
}

func (tx *DeclarationTransformer) appendCjsExports(combinedStatements *ast.StatementList) *ast.StatementList {
	result := []*ast.Node{}
	if tx.cjsExportAssignment != nil {
		result = append(result, tx.cjsExportAssignment)
	}
	result = append(result, tx.cjsExportMembers...)
	result = append(result, combinedStatements.Nodes...)
	statementNodes := flattenSyntaxLists(result)
	if len(statementNodes) != len(combinedStatements.Nodes) {
		combinedStatements = tx.Factory().NewNodeList(statementNodes)
	}
	return combinedStatements
}

func (tx *DeclarationTransformer) transformSourceFile(node *ast.SourceFile) *ast.Node {
	tx.cjsExportAssignment = nil
	tx.cjsExportAssignmentName = nil
	tx.cjsExportMembers = nil
	defer func() {
		tx.cjsExportAssignment = nil
		tx.cjsExportAssignmentName = nil
		tx.cjsExportMembers = nil
	}()
	tx.cjsExportAssignmentVisitor.VisitNode(node.AsNode()) // collect nested module.exports= assignments
	tx.expressionVisitor.VisitNode(node.AsNode())          // collect expando members (requires any export assignment be located in advance)
	var combinedStatements *ast.StatementList
	statements := tx.Visitor().VisitNodes(node.Statements)
	combinedStatements = tx.transformAndReplaceLatePaintedStatements(statements)
	combinedStatements = tx.appendCjsExports(combinedStatements)
	combinedStatements = tx.createEffectSchemaSourceFileDeclarations(combinedStatements)
	combinedStatements.Loc = statements.Loc // setTextRange
	if ast.IsExternalOrCommonJSModule(node) {
		if ast.IsInJSFile(node.AsNode()) {
			if exportEquals := node.Symbol.Exports[ast.InternalSymbolNameExportEquals]; exportEquals != nil && len(exportEquals.Declarations) > 1 {
				for _, node := range exportEquals.Declarations {
					tx.state.addDiagnostic(createDiagnosticForNode(node, diagnostics.Multiple_module_exports_assignments_cannot_be_serialized_for_declaration_emit))
				}
			}
		}
		if !tx.resultHasExternalModuleIndicator || (tx.needsScopeFixMarker && !tx.resultHasScopeMarker) {
			marker := createEmptyExports(tx.Factory().AsNodeFactory())
			newList := append(combinedStatements.Nodes, marker)
			withMarker := tx.Factory().NewNodeList(newList)
			withMarker.Loc = combinedStatements.Loc
			combinedStatements = withMarker
		}
	}
	outputFilePath := tspath.GetDirectoryPath(tspath.NormalizeSlashes(tx.declarationFilePath))
	result := tx.Factory().UpdateSourceFile(node, combinedStatements, node.EndOfFileToken)
	result.AsSourceFile().LibReferenceDirectives = tx.getLibReferences()
	result.AsSourceFile().TypeReferenceDirectives = tx.getTypeReferences()
	result.AsSourceFile().IsDeclarationFile = true
	result.AsSourceFile().ReferencedFiles = tx.getReferencedFiles(outputFilePath)
	return result.AsNode()
}

func createEmptyExports(factory *ast.NodeFactory) *ast.Node {
	return factory.NewExportDeclaration(nil /*isTypeOnly*/, false, factory.NewNamedExports(factory.NewNodeList([]*ast.Node{})), nil, nil)
}

func (tx *DeclarationTransformer) transformAndReplaceLatePaintedStatements(statements *ast.StatementList) *ast.StatementList {
	// This is a `while` loop because `handleSymbolAccessibilityError` can see additional import aliases marked as visible during
	// error handling which must now be included in the output and themselves checked for errors.
	// For example:
	// ```
	// module A {
	//   export module Q {}
	//   import B = Q;
	//   import C = B;
	//   export import D = C;
	// }
	// ```
	// In such a scenario, only Q and D are initially visible, but we don't consider imports as private names - instead we say they if they are referenced they must
	// be recorded. So while checking D's visibility we mark C as visible, then we must check C which in turn marks B, completing the chain of
	// dependent imports and allowing a valid declaration file output. Today, this dependent alias marking only happens for internal import aliases.
	for true {
		if len(tx.state.lateMarkedStatements) == 0 {
			break
		}

		next := tx.state.lateMarkedStatements[0]
		tx.state.lateMarkedStatements = tx.state.lateMarkedStatements[1:]

		saveNeedsDeclare := tx.needsDeclare
		tx.needsDeclare = next.Parent != nil && ast.IsSourceFile(next.Parent)

		result := tx.transformTopLevelDeclaration(next)

		tx.needsDeclare = saveNeedsDeclare
		original := tx.EmitContext().MostOriginal(next)
		id := ast.GetNodeId(original)
		tx.lateStatementReplacementMap[id] = result
	}

	// And lastly, we need to get the final form of all those indetermine import declarations from before and add them to the output list
	// (and remove them from the set to examine for outter declarations)
	results := make([]*ast.Node, 0, len(statements.Nodes))
	for _, statement := range statements.Nodes {
		if !ast.IsLateVisibilityPaintedStatement(statement) {
			results = append(results, statement)
			continue
		}
		original := tx.EmitContext().MostOriginal(statement)
		id := ast.GetNodeId(original)
		replacement, ok := tx.lateStatementReplacementMap[id]
		if !ok {
			results = append(results, statement)
			continue // not replaced
		}
		if replacement == nil {
			continue // deleted
		}
		if replacement.Kind == ast.KindSyntaxList {
			if !tx.needsScopeFixMarker || !tx.resultHasExternalModuleIndicator {
				for _, elem := range replacement.AsSyntaxList().Children {
					if needsScopeMarker(elem) {
						tx.needsScopeFixMarker = true
					}
					if ast.IsSourceFile(statement.Parent) && ast.IsExternalModuleIndicator(elem) {
						tx.resultHasExternalModuleIndicator = true
					}
				}
			}
			results = append(results, replacement.AsSyntaxList().Children...)
		} else {
			if needsScopeMarker(replacement) {
				tx.needsScopeFixMarker = true
			}
			if ast.IsSourceFile(statement.Parent) && ast.IsExternalModuleIndicator(replacement) {
				tx.resultHasExternalModuleIndicator = true
			}
			results = append(results, replacement)
		}
	}

	return tx.Factory().NewNodeList(results)
}

func (tx *DeclarationTransformer) getReferencedFiles(outputFilePath string) (results []*ast.FileReference) {
	// Handle path rewrites for triple slash ref comments
	for _, pair := range tx.rawReferencedFiles {
		sourceFile := pair.file
		ref := pair.ref

		if !ref.Preserve {
			continue
		}

		file := tx.host.GetSourceFileFromReference(sourceFile, ref)
		if file == nil {
			continue
		}

		var declFileName string
		if file.IsDeclarationFile {
			declFileName = file.FileName()
		} else {
			paths := tx.host.GetOutputPathsFor(file, true)
			// Try to use output path for referenced file, or output js path if that doesn't exist, or the input path if all else fails
			declFileName = paths.DeclarationFilePath()
			if len(declFileName) == 0 {
				declFileName = paths.JsFilePath()
			}
			if len(declFileName) == 0 {
				declFileName = file.FileName()
			}
		}
		// Should only be missing if the source file is missing a fileName (at which point we can't name a reference to it anyway)
		// TODO: Shouldn't this be a crash or assert instead of a silent continue?
		if len(declFileName) == 0 {
			continue
		}

		fileName := tspath.GetRelativePathToDirectoryOrUrl(
			outputFilePath,
			declFileName,
			false, // TODO: Probably unsafe to assume this isn't a URL, but that's what strada does
			tspath.ComparePathsOptions{
				CurrentDirectory:          tx.host.GetCurrentDirectory(),
				UseCaseSensitiveFileNames: tx.host.UseCaseSensitiveFileNames(),
			},
		)

		results = append(results, &ast.FileReference{
			TextRange:      core.NewTextRange(-1, -1),
			FileName:       fileName,
			ResolutionMode: ref.ResolutionMode,
			Preserve:       ref.Preserve,
		})
	}
	return results
}

func (tx *DeclarationTransformer) getLibReferences() (result []*ast.FileReference) {
	// clone retained references
	for _, ref := range tx.rawLibReferenceDirectives {
		if !ref.Preserve {
			continue
		}
		result = append(result, &ast.FileReference{
			TextRange:      core.NewTextRange(-1, -1),
			FileName:       ref.FileName,
			ResolutionMode: ref.ResolutionMode,
			Preserve:       ref.Preserve,
		})
	}
	return result
}

func (tx *DeclarationTransformer) getTypeReferences() (result []*ast.FileReference) {
	// clone retained references
	for _, ref := range tx.rawTypeReferenceDirectives {
		if !ref.Preserve {
			continue
		}
		result = append(result, &ast.FileReference{
			TextRange:      core.NewTextRange(-1, -1),
			FileName:       ref.FileName,
			ResolutionMode: ref.ResolutionMode,
			Preserve:       ref.Preserve,
		})
	}
	return result
}

func (tx *DeclarationTransformer) setupDiagnosticContext(input *ast.Node) (bool, func()) {
	canProdiceDiagnostic := canProduceDiagnostics(input)
	oldWithinObjectLiteralType := tx.suppressNewDiagnosticContexts
	shouldEnterSuppressNewDiagnosticsContextContext := (input.Kind == ast.KindTypeLiteral || input.Kind == ast.KindMappedType) && !(input.Parent.Kind == ast.KindTypeAliasDeclaration || input.Parent.Kind == ast.KindJSTypeAliasDeclaration)

	oldDiag := tx.state.getSymbolAccessibilityDiagnostic
	if canProdiceDiagnostic && !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(input)
	}
	oldName := tx.state.errorNameNode

	if shouldEnterSuppressNewDiagnosticsContextContext {
		tx.suppressNewDiagnosticContexts = true
	}

	return canProdiceDiagnostic, func() {
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
		tx.state.errorNameNode = oldName
		tx.suppressNewDiagnosticContexts = oldWithinObjectLiteralType
	}
}

func (tx *DeclarationTransformer) visitDeclarationSubtree(input *ast.Node) *ast.Node {
	if tx.shouldStripInternal(input) {
		return nil
	}
	if ast.IsDeclaration(input) {
		if isDeclarationAndNotVisible(tx.EmitContext(), tx.resolver, input) {
			return nil
		}
		if ast.HasDynamicName(input) {
			if tx.state.isolatedDeclarations {
				// Classes and object literals usually elide properties with computed names that are not of a literal type
				// In isolated declarations TSC needs to error on these as we don't know the type in a DTE.
				if !tx.resolver.IsDefinitelyReferenceToGlobalSymbolObject(input.Name().Expression()) {
					if ast.IsClassDeclaration(input.Parent) || ast.IsObjectLiteralExpression(input.Parent) {
						tx.state.addDiagnostic(createDiagnosticForNode(input, diagnostics.Computed_property_names_on_class_or_object_literals_cannot_be_inferred_with_isolatedDeclarations))
						return nil
					} else if (ast.IsInterfaceDeclaration(input.Parent) || ast.IsTypeLiteralNode(input.Parent)) && !ast.IsEntityNameExpression(input.Name().Expression()) {
						// Type declarations just need to double-check that the input computed name is an entity name expression
						tx.state.addDiagnostic(createDiagnosticForNode(input, diagnostics.Computed_properties_must_be_number_or_string_literals_variables_or_dotted_expressions_with_isolatedDeclarations))
						return nil
					}
				}
			} else if !tx.resolver.IsLateBound(tx.EmitContext().ParseNode(input)) || !ast.IsEntityNameExpression(input.Name().Expression()) {
				return nil
			}
		}
	}

	// Elide implementation signatures from overload sets
	if ast.IsFunctionLike(input) && tx.resolver.IsImplementationOfOverload(input) {
		return nil
	}

	if input.Kind == ast.KindSemicolonClassElement {
		return nil
	}

	if ast.IsHeritageClause(input) && (len(input.AsHeritageClause().Types.Nodes) == 0 || (len(input.AsHeritageClause().Types.Nodes) == 1 && ast.NodeIsMissing(input.AsHeritageClause().Types.Nodes[0]))) {
		return nil
	}

	previousEnclosingDeclaration := tx.enclosingDeclaration
	if isEnclosingDeclaration(input) {
		tx.enclosingDeclaration = input
	}

	canProdiceDiagnostic, cleanupDiagnosticContext := tx.setupDiagnosticContext(input)
	defer cleanupDiagnosticContext()

	var result *ast.Node

	switch input.Kind {
	case ast.KindMappedType:
		result = tx.transformMappedTypeNode(input.AsMappedTypeNode())
	case ast.KindHeritageClause:
		result = tx.transformHeritageClause(input.AsHeritageClause())
	case ast.KindMethodSignature:
		result = tx.transformMethodSignatureDeclaration(input.AsMethodSignatureDeclaration())
	case ast.KindMethodDeclaration:
		result = tx.transformMethodDeclaration(input.AsMethodDeclaration())
	case ast.KindConstructSignature:
		result = tx.transformConstructSignatureDeclaration(input.AsConstructSignatureDeclaration())
	case ast.KindConstructor:
		result = tx.transformConstructorDeclaration(input.AsConstructorDeclaration())
	case ast.KindGetAccessor:
		result = tx.transformGetAccesorDeclaration(input.AsGetAccessorDeclaration())
	case ast.KindSetAccessor:
		result = tx.transformSetAccessorDeclaration(input.AsSetAccessorDeclaration())
	case ast.KindPropertyDeclaration:
		result = tx.transformPropertyDeclaration(input.AsPropertyDeclaration())
	case ast.KindPropertySignature:
		result = tx.transformPropertySignatureDeclaration(input.AsPropertySignatureDeclaration())
	case ast.KindCallSignature:
		result = tx.transformCallSignatureDeclaration(input.AsCallSignatureDeclaration())
	case ast.KindIndexSignature:
		result = tx.transformIndexSignatureDeclaration(input.AsIndexSignatureDeclaration())
	case ast.KindVariableDeclaration:
		result = tx.transformVariableDeclaration(input.AsVariableDeclaration())
	case ast.KindTypeParameter:
		result = tx.transformTypeParameterDeclaration(input.AsTypeParameterDeclaration())
	case ast.KindExpressionWithTypeArguments:
		result = tx.transformExpressionWithTypeArguments(input.AsExpressionWithTypeArguments())
	case ast.KindTypeReference:
		result = tx.transformTypeReference(input.AsTypeReferenceNode())
	case ast.KindConditionalType:
		result = tx.transformConditionalTypeNode(input.AsConditionalTypeNode())
	case ast.KindFunctionType:
		result = tx.transformFunctionTypeNode(input.AsFunctionTypeNode())
	case ast.KindConstructorType:
		result = tx.transformConstructorTypeNode(input.AsConstructorTypeNode())
	case ast.KindImportType:
		result = tx.transformImportTypeNode(input.AsImportTypeNode())
	case ast.KindTypeQuery:
		tx.checkEntityNameVisibility(input.AsTypeQueryNode().ExprName, tx.enclosingDeclaration)
		result = tx.Visitor().VisitEachChild(input)
	case ast.KindTupleType:
		result = tx.Visitor().VisitEachChild(input)
		if result != nil {
			if transformers.IsOriginalNodeSingleLine(tx.EmitContext(), input) {
				tx.EmitContext().AddEmitFlags(result, printer.EFSingleLine)
			}
		}
	case ast.KindJSDocTypeExpression:
		result = tx.transformJSDocTypeExpression(input.AsJSDocTypeExpression())
	case ast.KindJSDocTypeLiteral:
		result = tx.transformJSDocTypeLiteral(input.AsJSDocTypeLiteral())
	case ast.KindJSDocPropertyTag:
		result = tx.transformJSDocPropertyTag(input.AsJSDocParameterOrPropertyTag())
	case ast.KindJSDocAllType:
		result = tx.transformJSDocAllType(input.AsJSDocAllType())
	case ast.KindJSDocNullableType:
		result = tx.transformJSDocNullableType(input.AsJSDocNullableType())
	case ast.KindJSDocNonNullableType:
		result = tx.transformJSDocNonNullableType(input.AsJSDocNonNullableType())
	case ast.KindJSDocOptionalType:
		result = tx.transformJSDocOptionalType(input.AsJSDocOptionalType())
	case ast.KindJSDocVariadicType:
		result = tx.transformJSDocVariadicType(input.AsJSDocVariadicType())
	default:
		result = tx.Visitor().VisitEachChild(input)
	}

	if result != nil && canProdiceDiagnostic && ast.HasDynamicName(input) {
		tx.checkName(input)
	}

	tx.enclosingDeclaration = previousEnclosingDeclaration
	return result
}

func (tx *DeclarationTransformer) checkName(node *ast.Node) {
	oldDiag := tx.state.getSymbolAccessibilityDiagnostic
	if !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNodeName(node)
	}
	tx.state.errorNameNode = node.Name()
	debug.Assert(ast.HasDynamicName(node)) // Should only be called with dynamic names
	entityName := node.Name().Expression()
	tx.checkEntityNameVisibility(entityName, tx.enclosingDeclaration)
	if !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	}
	tx.state.errorNameNode = nil
}

func (tx *DeclarationTransformer) transformMappedTypeNode(input *ast.MappedTypeNode) *ast.Node {
	// handle missing template type nodes, since the printer does not
	var typeNode *ast.Node
	if input.Type == nil {
		typeNode = tx.Factory().NewKeywordTypeNode(ast.KindAnyKeyword)
	} else {
		typeNode = tx.Visitor().Visit(input.Type)
	}
	return tx.Factory().UpdateMappedTypeNode(
		input,
		input.ReadonlyToken,
		tx.Visitor().Visit(input.TypeParameter),
		tx.Visitor().Visit(input.NameType),
		input.QuestionToken,
		typeNode,
		nil,
	)
}

func (tx *DeclarationTransformer) transformHeritageClause(clause *ast.HeritageClause) *ast.Node {
	retainedClauses := core.Filter(clause.Types.Nodes, func(t *ast.Node) bool {
		return ast.IsEntityNameExpression(t.AsExpressionWithTypeArguments().Expression) ||
			(clause.Token == ast.KindExtendsKeyword && t.Expression().Kind == ast.KindNullKeyword)
	})
	if len(retainedClauses) == 0 {
		return nil // elide empty clause
	}
	if len(retainedClauses) == len(clause.Types.Nodes) {
		return tx.Visitor().VisitEachChild(clause.AsNode())
	}
	return tx.Factory().UpdateHeritageClause(
		clause,
		clause.Token,
		tx.Visitor().VisitNodes(tx.Factory().NewNodeList(retainedClauses)),
	)
}

func (tx *DeclarationTransformer) transformImportTypeNode(input *ast.ImportTypeNode) *ast.Node {
	if !ast.IsLiteralImportTypeNode(input.AsNode()) {
		return input.AsNode()
	}
	return tx.Factory().UpdateImportTypeNode(
		input,
		input.IsTypeOf,
		tx.Factory().UpdateLiteralTypeNode(
			input.Argument.AsLiteralTypeNode(),
			tx.rewriteModuleSpecifier(input.AsNode(), input.Argument.AsLiteralTypeNode().Literal),
		),
		input.Attributes,
		input.Qualifier,
		tx.Visitor().VisitNodes(input.TypeArguments),
	)
}

func (tx *DeclarationTransformer) transformConstructorTypeNode(input *ast.ConstructorTypeNode) *ast.Node {
	return tx.Factory().UpdateConstructorTypeNode(
		input,
		tx.ensureModifiers(input.AsNode()),
		tx.Visitor().VisitNodes(input.TypeParameters),
		tx.updateParamList(input.AsNode(), input.Parameters),
		tx.Visitor().Visit(input.Type),
	)
}

func (tx *DeclarationTransformer) transformFunctionTypeNode(input *ast.FunctionTypeNode) *ast.Node {
	return tx.Factory().UpdateFunctionTypeNode(
		input,
		tx.Visitor().VisitNodes(input.TypeParameters),
		tx.updateParamList(input.AsNode(), input.Parameters),
		tx.Visitor().Visit(input.Type),
	)
}

func (tx *DeclarationTransformer) transformConditionalTypeNode(input *ast.ConditionalTypeNode) *ast.Node {
	checkType := tx.Visitor().Visit(input.CheckType)
	extendsType := tx.Visitor().Visit(input.ExtendsType)
	oldEnclosingDecl := tx.enclosingDeclaration
	tx.enclosingDeclaration = input.TrueType
	trueType := tx.Visitor().Visit(input.TrueType)
	tx.enclosingDeclaration = oldEnclosingDecl
	falseType := tx.Visitor().Visit(input.FalseType)

	return tx.Factory().UpdateConditionalTypeNode(
		input,
		checkType,
		extendsType,
		trueType,
		falseType,
	)
}

func (tx *DeclarationTransformer) transformTypeReference(input *ast.TypeReferenceNode) *ast.Node {
	tx.checkEntityNameVisibility(input.TypeName, tx.enclosingDeclaration)
	return tx.Visitor().VisitEachChild(input.AsNode())
}

func (tx *DeclarationTransformer) transformExpressionWithTypeArguments(input *ast.ExpressionWithTypeArguments) *ast.Node {
	if ast.IsEntityName(input.Expression) || ast.IsEntityNameExpression(input.Expression) {
		tx.checkEntityNameVisibility(input.Expression, tx.enclosingDeclaration)
	}
	return tx.Visitor().VisitEachChild(input.AsNode())
}

func (tx *DeclarationTransformer) transformTypeParameterDeclaration(input *ast.TypeParameterDeclaration) *ast.Node {
	if isPrivateMethodTypeParameter(tx.host, input) && (input.DefaultType != nil || input.Constraint != nil) {
		return tx.Factory().UpdateTypeParameterDeclaration(
			input,
			input.Modifiers(),
			input.Name(),
			nil,
			input.Expression,
			nil,
		)
	}
	return tx.Visitor().VisitEachChild(input.AsNode())
}

func (tx *DeclarationTransformer) transformVariableDeclaration(input *ast.VariableDeclaration) *ast.Node {
	if tx.state.currentSourceFile.CommonJSModuleIndicator != nil && ast.IsVariableDeclarationInitializedToRequire(input.AsNode()) {
		return tx.transformCjsRequireVariableDeclaration(input)
	}
	if ast.IsBindingPattern(input.Name()) {
		return tx.recreateBindingPattern(input.Name().AsBindingPattern())
	}
	// Variable declaration types also suppress new diagnostic contexts, provided the contexts wouldn't be made for binding pattern types
	tx.suppressNewDiagnosticContexts = true
	return tx.Factory().UpdateVariableDeclaration(
		input,
		input.Name(),
		nil,
		tx.ensureType(input.AsNode(), false),
		tx.ensureNoInitializer(input.AsNode()),
	)
}

func (tx *DeclarationTransformer) transformCjsRequireVariableDeclaration(input *ast.VariableDeclaration) *ast.Node {
	specifier := tx.rewriteModuleSpecifier(input.AsNode(), input.Initializer.AsCallExpression().Arguments.Nodes[0])
	if ast.IsIdentifier(input.Name()) {
		// `const x = require("something")` -> `import x = require("something")`
		return tx.Factory().NewImportEqualsDeclaration(nil, false, input.Name(), tx.Factory().NewExternalModuleReference(specifier))
	} else if ast.IsArrayBindingPattern(input.Name()) {
		// TODO: Is this actually reachable? should we error on this?
		return nil
	} else { // object binding pattern

		// `const {x, y: z} = require("something")` -> `import {x, y as z} from "something"`
		b := input.Name().AsBindingPattern()
		var importSpecifiers []*ast.Node
		for _, elem := range b.Elements.Nodes {
			if !ast.IsIdentifier(elem.Name()) {
				continue // nested destructuring, bail
			}
			importSpecifiers = append(importSpecifiers, tx.Factory().NewImportSpecifier(false, elem.PropertyName(), elem.Name()))
		}
		return tx.Factory().NewImportDeclaration(
			nil,
			tx.Factory().NewImportClause(
				ast.KindUnknown,
				nil,
				tx.Factory().NewNamedImports(tx.Factory().NewNodeList(importSpecifiers)),
			),
			specifier,
			nil,
		)
	}
}

func (tx *DeclarationTransformer) recreateBindingPattern(input *ast.BindingPattern) *ast.Node {
	var results []*ast.Node
	for _, elem := range input.Elements.Nodes {
		result := tx.recreateBindingElement(elem.AsBindingElement())
		if result == nil {
			continue
		}
		if result.Kind == ast.KindSyntaxList {
			results = append(results, result.AsSyntaxList().Children...)
		} else {
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		return nil
	}
	if len(results) == 1 {
		return results[0]
	}
	return tx.Factory().NewSyntaxList(results)
}

func (tx *DeclarationTransformer) recreateBindingElement(e *ast.BindingElement) *ast.Node {
	if e.Name() == nil {
		return nil
	}
	if !getBindingNameVisible(tx.resolver, e.AsNode()) {
		return nil
	}
	if ast.IsBindingPattern(e.Name()) {
		return tx.recreateBindingPattern(e.Name().AsBindingPattern())
	}
	return tx.Factory().NewVariableDeclaration(
		e.Name(),
		nil,
		tx.ensureType(e.AsNode(), false),
		nil, // TODO: possible strada bug - not emitting const initialized binding pattern elements?
	)
}

func (tx *DeclarationTransformer) transformIndexSignatureDeclaration(input *ast.IndexSignatureDeclaration) *ast.Node {
	t := tx.Visitor().Visit(input.Type)
	if t == nil {
		t = tx.Factory().NewKeywordTypeNode(ast.KindAnyKeyword)
	}
	return tx.Factory().UpdateIndexSignatureDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		tx.updateParamList(input.AsNode(), input.Parameters),
		t,
	)
}

func (tx *DeclarationTransformer) transformCallSignatureDeclaration(input *ast.CallSignatureDeclaration) *ast.Node {
	return tx.Factory().UpdateCallSignatureDeclaration(
		input,
		tx.ensureTypeParams(input.AsNode(), input.TypeParameters),
		tx.updateParamList(input.AsNode(), input.Parameters),
		tx.ensureType(input.AsNode(), false),
	)
}

func (tx *DeclarationTransformer) transformPropertySignatureDeclaration(input *ast.PropertySignatureDeclaration) *ast.Node {
	if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	}
	result := tx.Factory().UpdatePropertySignatureDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		input.PostfixToken,
		tx.ensureType(input.AsNode(), false),
		tx.ensureNoInitializer(input.AsNode()), // TODO: possible strada bug (fixed here) - const property signatures never initialized
	)
	tx.preservePartialJsDoc(result, input.AsNode())
	return result
}

func (tx *DeclarationTransformer) transformPropertyDeclaration(input *ast.PropertyDeclaration) *ast.Node {
	if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	}
	// Remove definite assignment assertion (!) from declaration files
	postfixToken := input.PostfixToken
	if postfixToken != nil && postfixToken.Kind == ast.KindExclamationToken {
		postfixToken = nil
	}
	return tx.Factory().UpdatePropertyDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		postfixToken,
		tx.ensureType(input.AsNode(), false),
		tx.ensureNoInitializer(input.AsNode()),
	)
}

func (tx *DeclarationTransformer) transformSetAccessorDeclaration(input *ast.SetAccessorDeclaration) *ast.Node {
	if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	}

	return tx.Factory().UpdateSetAccessorDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		nil, // accessors shouldn't have type params
		tx.updateAccessorParamList(input.AsNode(), tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(input.AsNode()), ast.ModifierFlagsPrivate) != 0),
		nil,
		nil,
		nil,
	)
}

func (tx *DeclarationTransformer) transformGetAccesorDeclaration(input *ast.GetAccessorDeclaration) *ast.Node {
	if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	}
	return tx.Factory().UpdateGetAccessorDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		nil, // accessors shouldn't have type params
		tx.updateAccessorParamList(input.AsNode(), tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(input.AsNode()), ast.ModifierFlagsPrivate) != 0),
		tx.ensureType(input.AsNode(), false),
		nil,
		nil,
	)
}

func (tx *DeclarationTransformer) updateAccessorParamList(input *ast.Node, isPrivate bool) *ast.ParameterList {
	var newParams []*ast.Node
	if !isPrivate {
		thisParam := ast.GetThisParameter(input)
		if thisParam != nil {
			newParams = append(newParams, tx.ensureParameter(thisParam.AsParameterDeclaration()))
		}
	}
	if ast.IsSetAccessorDeclaration(input) {
		var valueParam *ast.Node
		if !isPrivate {
			if len(newParams) == 1 && len(input.AsSetAccessorDeclaration().Parameters.Nodes) >= 2 {
				valueParam = tx.ensureParameter(input.AsSetAccessorDeclaration().Parameters.Nodes[1].AsParameterDeclaration())
			} else if len(newParams) == 0 && len(input.AsSetAccessorDeclaration().Parameters.Nodes) >= 1 {
				valueParam = tx.ensureParameter(input.AsSetAccessorDeclaration().Parameters.Nodes[0].AsParameterDeclaration())
			}
		}
		if valueParam == nil {
			// When synthesizing a missing value parameter, emit `value: any` for non-private accessors to match TypeScript's declaration emit behavior.
			var t *ast.Node
			if !isPrivate {
				t = tx.Factory().NewKeywordTypeNode(ast.KindAnyKeyword)
			}
			valueParam = tx.Factory().NewParameterDeclaration(
				nil,
				nil,
				tx.Factory().NewIdentifier("value"),
				nil,
				t,
				nil,
			)
		}
		newParams = append(newParams, valueParam)
	}
	return tx.Factory().NewNodeList(newParams)
}

func (tx *DeclarationTransformer) transformConstructorDeclaration(input *ast.ConstructorDeclaration) *ast.Node {
	// A constructor declaration may not have a type annotation
	return tx.Factory().UpdateConstructorDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		nil, // no type params
		tx.updateParamList(input.AsNode(), input.Parameters),
		nil, // no return type
		nil,
		nil,
	)
}

func (tx *DeclarationTransformer) transformConstructSignatureDeclaration(input *ast.ConstructSignatureDeclaration) *ast.Node {
	return tx.Factory().UpdateConstructSignatureDeclaration(
		input,
		tx.ensureTypeParams(input.AsNode(), input.TypeParameters),
		tx.updateParamList(input.AsNode(), input.Parameters),
		tx.ensureType(input.AsNode(), false),
	)
}

func (tx *DeclarationTransformer) omitPrivateMethodType(input *ast.Node) *ast.Node {
	if input.Symbol() != nil && len(input.Symbol().Declarations) > 0 && input.Symbol().Declarations[0] != input {
		return nil
	}
	result := tx.Factory().NewPropertyDeclaration(
		tx.ensureModifiers(input),
		input.Name(),
		nil,
		nil,
		nil,
	)
	tx.preserveJsDoc(result, input)
	return result
}

func (tx *DeclarationTransformer) transformMethodSignatureDeclaration(input *ast.MethodSignatureDeclaration) *ast.Node {
	if tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(input.AsNode()), ast.ModifierFlagsPrivate) != 0 {
		return tx.omitPrivateMethodType(input.AsNode())
	} else if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	} else {
		return tx.Factory().UpdateMethodSignatureDeclaration(
			input,
			tx.ensureModifiers(input.AsNode()),
			input.Name(),
			input.PostfixToken,
			tx.ensureTypeParams(input.AsNode(), input.TypeParameters),
			tx.updateParamList(input.AsNode(), input.Parameters),
			tx.ensureType(input.AsNode(), false),
		)
	}
}

func (tx *DeclarationTransformer) transformMethodDeclaration(input *ast.MethodDeclaration) *ast.Node {
	if tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(input.AsNode()), ast.ModifierFlagsPrivate) != 0 {
		return tx.omitPrivateMethodType(input.AsNode())
	} else if ast.IsPrivateIdentifier(input.Name()) {
		return nil
	} else {
		return tx.Factory().UpdateMethodDeclaration(
			input,
			tx.ensureModifiers(input.AsNode()),
			nil,
			input.Name(),
			input.PostfixToken,
			tx.ensureTypeParams(input.AsNode(), input.TypeParameters),
			tx.updateParamList(input.AsNode(), input.Parameters),
			tx.ensureType(input.AsNode(), false),
			nil,
			nil,
		)
	}
}

func (tx *DeclarationTransformer) visitDeclarationStatements(input *ast.Node) *ast.Node {
	if tx.shouldStripInternal(input) {
		return nil
	}
	switch input.Kind {
	case ast.KindExportDeclaration:
		if ast.IsSourceFile(input.Parent) {
			tx.resultHasExternalModuleIndicator = true
		}
		tx.resultHasScopeMarker = true
		// Rewrite external module names if necessary
		return tx.Factory().UpdateExportDeclaration(
			input.AsExportDeclaration(),
			input.Modifiers(),
			input.IsTypeOnly(),
			input.AsExportDeclaration().ExportClause,
			tx.rewriteModuleSpecifier(input, input.ModuleSpecifier()),
			tx.tryGetResolutionModeOverride(input.AsExportDeclaration().Attributes),
		)
	case ast.KindExportAssignment:
		return tx.transformExportAssignment(input, input, input.Expression(), input.AsExportAssignment().IsExportEquals)
	default:
		id := ast.GetNodeId(tx.EmitContext().MostOriginal(input))
		if tx.lateStatementReplacementMap[id] == nil {
			// Don't actually transform yet; just leave as original node - will be elided/swapped by late pass
			tx.lateStatementReplacementMap[id] = tx.transformTopLevelDeclaration(input)
		}
		return input
	}
}

func (tx *DeclarationTransformer) tryGetNameOfAssignedExpression(unwrapped *ast.Node) *ast.Node {
	var nameNode *ast.Node
	var nameText string
	if !ast.IsPropertyAccessExpression(unwrapped) && unwrapped.Name() != nil {
		nameText = unwrapped.Name().Text()
	} else if ast.IsIdentifier(unwrapped) {
		nameText = unwrapped.Text()
	}
	if nameText != "" && nameText != "default" {
		if tx.resolver.IsNameResolvable(tx.enclosingDeclaration, nameText) {
			// create a unique name that shares the same text as its' base
			nameNode = tx.Factory().NewUniqueNameEx(nameText, printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
		} else {
			// use the node's name as-is, since it's not otherwise in-scope
			nameNode = tx.Factory().NewIdentifier(nameText)
		}
	}
	return nameNode
}

func (tx *DeclarationTransformer) getNameOfExportedAssignedExpression(unwrapped *ast.Node, isExportEquals bool) *ast.Node {
	nameNode := tx.tryGetNameOfAssignedExpression(unwrapped)
	if nameNode == nil {
		// fallback to a default name
		if isExportEquals && ast.IsSourceFileJS(tx.state.currentSourceFile) {
			// only JS files prefer to use `_exports` for export assignments - TS has always used `_default` for both `export=` and `export default`
			nameNode = tx.Factory().NewUniqueNameEx("_exports", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
		} else {
			nameNode = tx.Factory().NewUniqueNameEx("_default", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
		}
	}
	tx.cjsExportAssignmentName = nameNode
	return nameNode
}

func (tx *DeclarationTransformer) transformExportAssignment(input *ast.Node, assignment *ast.Node, expression *ast.Node, isExportEquals bool) *ast.Node {
	if ast.IsSourceFile(input.Parent) {
		tx.resultHasExternalModuleIndicator = true
	}
	tx.resultHasScopeMarker = true
	if ast.IsIdentifier(expression) && (ast.IsSourceFile(input.Parent) || ast.IsModuleBlock(input.Parent)) {
		exportAssignment := tx.Factory().NewExportAssignment(nil, isExportEquals, nil, expression)
		tx.preserveJsDoc(exportAssignment, input)
		return exportAssignment
	}

	// Check if the expression is a class expression - emit as a class declaration + export assignment
	unwrapped := ast.SkipOuterExpressions(expression, ast.OEKExpressionTypePassthrough)
	newId := tx.getNameOfExportedAssignedExpression(unwrapped, isExportEquals)
	if ast.IsClassExpression(unwrapped) {
		var mods []*ast.Node
		if tx.needsDeclare {
			mods = append(mods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
		}
		classDecl := tx.transformClassExpressionToDeclaration(unwrapped, newId, tx.Factory().NewModifierList(mods))
		tx.preserveJsDoc(classDecl, input)
		// Reuse the same name node for the export so unique names resolve consistently
		exportAssignment := tx.Factory().NewExportAssignment(nil, isExportEquals, nil, newId)
		tx.removeAllComments(exportAssignment)
		return tx.Factory().NewSyntaxList([]*ast.Node{exportAssignment, classDecl})
	} else if ast.IsFunctionLike(unwrapped) {
		// Promote function or arrow function expressions to a function declaration
		var mods []*ast.Node
		if tx.needsDeclare {
			mods = append(mods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
		}
		funcDecl := tx.transformFunctionLikeToDeclaration(unwrapped, newId, tx.Factory().NewModifierList(mods))
		tx.preserveJsDoc(funcDecl, input)
		// Reuse the same name node for the export so unique names resolve consistently
		exportAssignment := tx.Factory().NewExportAssignment(nil, isExportEquals, nil, newId)
		tx.removeAllComments(exportAssignment)
		return tx.Factory().NewSyntaxList([]*ast.Node{exportAssignment, funcDecl})
	}

	// expression is non-identifier, create _default typed variable to reference
	tx.state.getSymbolAccessibilityDiagnostic = func(_ printer.SymbolAccessibilityResult) *SymbolAccessibilityDiagnostic {
		return &SymbolAccessibilityDiagnostic{
			diagnosticMessage: diagnostics.Default_export_of_the_module_has_or_is_using_private_name_0,
			errorNode:         input,
		}
	}
	tx.cjsExportAssignmentName = newId
	tx.tracker.PushErrorFallbackNode(assignment)
	var type_, initializer *ast.Node
	if ast.IsPrimitiveLiteralValue(unwrapParenthesizedExpression(expression), true) {
		initializer = tx.resolver.CreateLiteralConstValue(tx.EmitContext(), tx.EmitContext().ParseNode(assignment), tx.tracker)
	}
	if initializer == nil {
		type_ = tx.ensureType(assignment, false)
	}
	varDecl := tx.Factory().NewVariableDeclaration(newId, nil, type_, initializer)
	tx.tracker.PopErrorFallbackNode()
	var modList *ast.ModifierList
	if tx.needsDeclare {
		modList = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindDeclareKeyword)})
	} else {
		modList = tx.Factory().NewModifierList([]*ast.Node{})
	}
	statement := tx.Factory().NewVariableStatement(modList, tx.Factory().NewVariableDeclarationList(tx.Factory().NewNodeList([]*ast.Node{varDecl}), ast.NodeFlagsConst))
	exportAssignment := tx.Factory().NewExportAssignment(nil, isExportEquals, nil, newId)
	// Remove comments from the export declaration and copy them onto the synthetic _default declaration
	tx.preserveJsDoc(statement, input)
	return tx.Factory().NewSyntaxList([]*ast.Node{statement, exportAssignment})
}

func (tx *DeclarationTransformer) transformFunctionLikeToDeclaration(unwrapped *ast.Node, funcName *ast.Node, mods *ast.ModifierList) *ast.Node {
	d := unwrapped.FunctionLikeData()
	return tx.Factory().NewFunctionDeclaration(
		mods,
		nil,
		funcName,
		tx.ensureTypeParams(unwrapped, d.TypeParameters),
		tx.updateParamList(unwrapped, d.Parameters),
		tx.ensureType(unwrapped, false),
		tx.Visitor().VisitNode(d.FullSignature),
		nil,
	)
}

func (tx *DeclarationTransformer) transformBinaryExpressionToExportDeclaration(input *ast.Node, name *ast.Node) *ast.Node {
	propertyName := input.AsBinaryExpression().Right

	// track alias target so referenced declarations are included in the output
	tx.tracker.handleSymbolAccessibilityError(tx.resolver.IsEntityNameVisible(propertyName, tx.enclosingDeclaration))

	if ast.IsIdentifier(name) && propertyName.Text() == name.Text() {
		propertyName = nil
	}

	return tx.Factory().NewExportDeclaration(
		nil,
		false,
		tx.Factory().NewNamedExports(tx.Factory().NewNodeList([]*ast.Node{tx.Factory().NewExportSpecifier(false, propertyName, name)})),
		nil,
		nil,
	)
}

func (tx *DeclarationTransformer) transformCommonJSExport(input *ast.Node, name *ast.Node) *ast.Node {
	res := tx.transformCommonJSExportWorker(input, name)
	if res == nil {
		return res
	}
	return tx.wrapInCJSExportNamespace(res)
}

func (tx *DeclarationTransformer) transformCommonJSExportWorker(input *ast.Node, name *ast.Node) *ast.Node {
	var nameText string
	if ast.IsIdentifier(name) || ast.IsStringLiteral(name) {
		nameText = name.Text()
	}
	if tx.witnessedCjsExports.Has(nameText) && nameText != "" {
		return nil // Already emitted this export name
	}
	tx.witnessedCjsExports.Add(nameText)
	tx.resultHasExternalModuleIndicator = true
	tx.resultHasScopeMarker = true
	// only transform cjs exports to shorthand at the top-level of a source file, otherwise we uniformly emit nested exports with a type annotation
	if isCommonJSAliasExport(input) && ast.IsExpressionStatement(input.Parent) && ast.IsSourceFile(input.Parent.Parent) {
		// export { name }
		// export { source as name }
		return tx.transformBinaryExpressionToExportDeclaration(input, name)
	}

	// Check if the RHS is a class expression - emit as a class declaration instead of a typed variable
	if ast.IsBinaryExpression(input) {
		if rhs := unwrapParenthesizedExpression(input.AsBinaryExpression().Right); ast.IsClassExpression(rhs) {
			ce := rhs.AsClassExpression()
			classExprName := ce.Name()
			hasExprName := classExprName != nil && len(classExprName.Text()) > 0

			if hasExprName {
				// Set up TrackSymbol watch to detect if the class expression's own
				// symbol is referenced during member type serialization.
				tx.tracker.watchedClassSymbol = rhs.Symbol()
				tx.tracker.classSymbolTracked = false
				defer func() {
					tx.tracker.watchedClassSymbol = nil
					tx.tracker.classSymbolTracked = false
				}()

				// Serialize class members using the class expression name, which
				// triggers TrackSymbol for any self-referential member types.
				className := tx.Factory().NewIdentifier(classExprName.Text())
				classMods := []*ast.Node{tx.Factory().NewModifier(ast.KindExportKeyword)}
				classDecl := tx.transformClassExpressionToDeclaration(rhs, className, tx.Factory().NewModifierList(classMods))
				tx.preserveJsDoc(classDecl, input)

				// Determine if namespace isolation is needed:
				// - The class expression name differs from the export name, OR
				// - The class's own symbol was used in a member's serialized type
				namesDiffer := !ast.IsIdentifier(name) || classExprName.Text() != name.Text()
				needsIsolation := namesDiffer || tx.tracker.classSymbolTracked

				if needsIsolation {
					nsName := tx.Factory().NewUniqueNameEx("_ns", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
					var nsMods []*ast.Node
					if tx.needsDeclare {
						nsMods = append(nsMods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
					}
					nsDecl := tx.Factory().NewModuleDeclaration(
						tx.Factory().NewModifierList(nsMods),
						ast.KindNamespaceKeyword,
						nsName,
						tx.Factory().NewModuleBlock(tx.Factory().NewNodeList([]*ast.Node{classDecl})),
					)

					aliasBase := "_exported"
					if nameText := name.Text(); ast.IsIdentifier(name) && scanner.IsIdentifierText("_"+nameText, core.LanguageVariantStandard) {
						aliasBase = "_" + nameText
					}
					importAlias := tx.Factory().NewUniqueNameEx(aliasBase, printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
					qualifiedName := tx.Factory().NewQualifiedName(nsName, className)
					importDecl := tx.Factory().NewImportEqualsDeclaration(nil, false, importAlias, qualifiedName)

					exportSpecifier := tx.Factory().NewExportSpecifier(false, importAlias, name)
					exportDecl := tx.Factory().NewExportDeclaration(nil, false, tx.Factory().NewNamedExports(tx.Factory().NewNodeList([]*ast.Node{exportSpecifier})), nil, nil)
					tx.removeAllComments(exportDecl)

					return tx.Factory().NewSyntaxList(append([]*ast.Node{nsDecl, importDecl}, exportDecl))
				}

				// No isolation needed: names match and no self-references.
				// Update modifiers to include declare if needed.
				var mods []*ast.Node
				mods = append(mods, tx.Factory().NewModifier(ast.KindExportKeyword))
				if tx.needsDeclare {
					mods = append(mods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
				}
				classDecl = tx.Factory().UpdateClassDeclaration(
					classDecl.AsClassDeclaration(),
					tx.Factory().NewModifierList(mods),
					classDecl.AsClassDeclaration().Name(),
					classDecl.AsClassDeclaration().TypeParameters,
					classDecl.AsClassDeclaration().HeritageClauses,
					classDecl.AsClassDeclaration().Members,
				)
				return classDecl
			}
			var mods []*ast.Node
			mods = append(mods, tx.Factory().NewModifier(ast.KindExportKeyword))
			if tx.needsDeclare {
				mods = append(mods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
			}
			className := name
			if !ast.IsIdentifier(className) {
				className = tx.Factory().NewUniqueNameEx("_class", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
			}
			classDecl := tx.transformClassExpressionToDeclaration(rhs, className, tx.Factory().NewModifierList(mods))
			tx.preserveJsDoc(classDecl, input)
			if !ast.IsIdentifier(name) {
				// Non-identifier name: emit class declaration + named export
				exportDecl := tx.Factory().NewExportDeclaration(nil, false, tx.Factory().NewNamedExports(tx.Factory().NewNodeList([]*ast.Node{tx.Factory().NewExportSpecifier(false, className, name)})), nil, nil)
				tx.removeAllComments(exportDecl)
				return tx.Factory().NewSyntaxList([]*ast.Node{classDecl, exportDecl})
			}
			return classDecl
		}
	}

	if ast.IsIdentifier(name) {
		if name.Text() == "default" {
			// const _default: Type; export default _default;
			newId := tx.Factory().NewUniqueNameEx("_default", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
			tx.state.getSymbolAccessibilityDiagnostic = func(_ printer.SymbolAccessibilityResult) *SymbolAccessibilityDiagnostic {
				return &SymbolAccessibilityDiagnostic{
					diagnosticMessage: diagnostics.Default_export_of_the_module_has_or_is_using_private_name_0,
					errorNode:         input,
				}
			}
			tx.tracker.PushErrorFallbackNode(input)
			type_ := tx.ensureType(input, false)
			varDecl := tx.Factory().NewVariableDeclaration(newId, nil, type_, nil)
			tx.tracker.PopErrorFallbackNode()
			var modList *ast.ModifierList
			if tx.needsDeclare {
				modList = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindDeclareKeyword)})
			} else {
				modList = tx.Factory().NewModifierList([]*ast.Node{})
			}
			statement := tx.Factory().NewVariableStatement(modList, tx.Factory().NewVariableDeclarationList(tx.Factory().NewNodeList([]*ast.Node{varDecl}), ast.NodeFlagsConst))

			assignment := tx.Factory().NewExportAssignment(input.Modifiers(), false, nil, newId)
			// Remove comments from the export declaration and copy them onto the synthetic _default declaration
			tx.preserveJsDoc(statement, input)
			tx.removeAllComments(assignment)
			return tx.Factory().NewSyntaxList([]*ast.Node{statement, assignment})
		} else if tx.host.GetEmitResolver().GetReferencedValueDeclaration(name) == input || tx.host.GetEmitResolver().GetReferencedValueDeclaration(name) == nil {
			// only inline to a export var if the `name` lookup points at this assignment or nothing - if it points at something else, we must use a temp name
			// export var name: Type
			tx.tracker.PushErrorFallbackNode(input)
			type_ := tx.ensureType(input, false)
			varDecl := tx.Factory().NewVariableDeclaration(name, nil, type_, nil)
			tx.tracker.PopErrorFallbackNode()
			var modList *ast.ModifierList
			if tx.needsDeclare {
				modList = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindExportKeyword), tx.Factory().NewModifier(ast.KindDeclareKeyword)})
			} else {
				modList = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindExportKeyword)})
			}
			return tx.Factory().NewVariableStatement(modList, tx.Factory().NewVariableDeclarationList(tx.Factory().NewNodeList([]*ast.Node{varDecl}), ast.NodeFlagsNone))
		}
	}
	// const _exported: Type; export {_exported as "name"};
	newId := tx.Factory().NewUniqueNameEx("_exported", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
	tx.state.getSymbolAccessibilityDiagnostic = func(_ printer.SymbolAccessibilityResult) *SymbolAccessibilityDiagnostic {
		return &SymbolAccessibilityDiagnostic{
			diagnosticMessage: diagnostics.Default_export_of_the_module_has_or_is_using_private_name_0,
			errorNode:         input,
		}
	}
	tx.tracker.PushErrorFallbackNode(input)
	type_ := tx.ensureType(input, false)
	varDecl := tx.Factory().NewVariableDeclaration(newId, nil, type_, nil)
	tx.tracker.PopErrorFallbackNode()
	var modList *ast.ModifierList
	if tx.needsDeclare {
		modList = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindDeclareKeyword)})
	} else {
		modList = tx.Factory().NewModifierList([]*ast.Node{})
	}
	statement := tx.Factory().NewVariableStatement(modList, tx.Factory().NewVariableDeclarationList(tx.Factory().NewNodeList([]*ast.Node{varDecl}), ast.NodeFlagsConst))

	assignment := tx.Factory().NewExportDeclaration(nil, false, tx.Factory().NewNamedExports(tx.Factory().NewNodeList([]*ast.Node{tx.Factory().NewExportSpecifier(false, newId, name)})), nil, nil)
	// Remove comments from the export declaration and copy them onto the synthetic _default declaration
	tx.preserveJsDoc(statement, input)
	tx.removeAllComments(assignment)
	return tx.Factory().NewSyntaxList([]*ast.Node{statement, assignment})
}

func (tx *DeclarationTransformer) wrapInCJSExportNamespace(content *ast.Node) *ast.Node {
	if tx.cjsExportAssignmentName == nil {
		return content
	}
	// Reuse the same name node so unique names resolve consistently with the class/export
	nsName := tx.cjsExportAssignmentName
	var members []*ast.Node
	if content.Kind == ast.KindSyntaxList {
		members = content.AsSyntaxList().Children
	} else {
		members = []*ast.Node{content}
	}
	var nsMods []*ast.Node
	if tx.needsDeclare {
		nsMods = append(nsMods, tx.Factory().NewModifier(ast.KindDeclareKeyword))
	}
	members, _ = tx.declareStrippingVisitor.VisitSlice(members)
	return tx.Factory().NewModuleDeclaration(
		tx.Factory().NewModifierList(nsMods),
		ast.KindNamespaceKeyword,
		nsName,
		tx.Factory().NewModuleBlock(tx.Factory().NewNodeList(members)),
	)
}

func isCommonJSAliasExport(node *ast.Node) bool {
	if ast.IsBinaryExpression(node) && ast.IsIdentifier(node.AsBinaryExpression().Right) {
		if symbol := node.Symbol(); symbol != nil && len(symbol.Declarations) == 1 {
			return true
		}
	}
	return false
}

// transformClassExpressionToDeclaration converts a class expression into a class declaration
// for use in CJS export declarations (e.g., exports.K = class K {} or module.exports = class Thing {}).
// This delegates to the shared buildClassMembers helper to stay in sync with transformClassDeclaration.
func (tx *DeclarationTransformer) transformClassExpressionToDeclaration(classExpr *ast.Node, className *ast.Node, modifiers *ast.ModifierList) *ast.Node {
	previousEnclosingDeclaration := tx.enclosingDeclaration
	tx.enclosingDeclaration = classExpr
	previousInClassExpressionDeclaration := tx.inClassExpressionDeclaration
	tx.inClassExpressionDeclaration = true
	defer func() {
		tx.enclosingDeclaration = previousEnclosingDeclaration
		tx.inClassExpressionDeclaration = previousInClassExpressionDeclaration
	}()

	var extraMembers []*ast.Node
	if ast.IsInJSFile(classExpr) {
		extraMembers = tx.collectThisPropertyAssignments(classExpr)
	}
	members := tx.buildClassMembers(classExpr, extraMembers...)
	typeParameters := tx.ensureTypeParams(classExpr, classExpr.AsClassExpression().TypeParameters)
	heritageClauses := tx.Visitor().VisitNodes(classExpr.AsClassExpression().HeritageClauses)

	return tx.Factory().NewClassDeclaration(
		modifiers,
		className,
		typeParameters,
		heritageClauses,
		members,
	)
}

func (tx *DeclarationTransformer) rewriteModuleSpecifier(parent *ast.Node, input *ast.Node) *ast.Node {
	if input == nil {
		return nil
	}
	tx.resultHasExternalModuleIndicator = tx.resultHasExternalModuleIndicator || (parent.Kind != ast.KindModuleDeclaration && parent.Kind != ast.KindImportType)
	return input
}

func (tx *DeclarationTransformer) tryGetResolutionModeOverride(node *ast.Node) *ast.Node {
	if node == nil {
		return node
	}
	mode := tx.host.GetResolutionModeOverride(node)
	if mode != core.ResolutionModeNone {
		return node
	}
	return nil
}

func (tx *DeclarationTransformer) preserveJsDoc(updated *ast.Node, original *ast.Node) {
	// Copy comment range from original to updated node so JSDoc comments are preserved
	tx.EmitContext().AssignCommentRange(updated, original)
}

func (tx *DeclarationTransformer) preservePartialJsDoc(updated *ast.Node, original *ast.Node) {
	if original.Flags&ast.NodeFlagsReparsed == 0 {
		return
	}
	jsdoc := core.FirstOrNil(original.EagerJSDoc(ast.GetSourceFileOfNode(original)))
	if jsdoc == nil {
		return
	}
	description := scanner.GetTextOfJSDocComment(jsdoc.AsJSDoc().Comment)
	if description == "" {
		return
	}
	comment := "*\n * " + strings.ReplaceAll(description, "\n", "\n * ") + "\n "
	tx.EmitContext().AddSyntheticLeadingComment(updated, ast.KindMultiLineCommentTrivia, comment, true /*hasTrailingNewLine*/)
}

func (tx *DeclarationTransformer) removeAllComments(node *ast.Node) {
	tx.EmitContext().AddEmitFlags(node, printer.EFNoComments)
	// !!! TODO: Also remove synthetic trailing/leading comments added by transforms
	// emitNode.leadingComments = undefined;
	// emitNode.trailingComments = undefined;
}

func (tx *DeclarationTransformer) ensureType(node *ast.Node, ignorePrivate bool) *ast.Node {
	if !ignorePrivate && tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(node), ast.ModifierFlagsPrivate) != 0 {
		// Private nodes emit no types (except private parameter properties, whose parameter types are actually visible)
		return nil
	}

	if tx.shouldPrintWithInitializer(node) {
		// Literal const declarations will have an initializer ensured rather than a type
		return nil
	}

	// Should be removed createTypeOfDeclaration will actually now reuse the existing annotation so there is no real need to duplicate type walking
	// Left in for now to minimize diff during syntactic type node builder refactor
	if !ast.IsExportAssignment(node) && !ast.IsBindingElement(node) && node.Type() != nil && (!ast.IsParameterDeclaration(node) || !tx.resolver.RequiresAddingImplicitUndefined(node, nil, tx.enclosingDeclaration)) {
		if tx.state.currentSourceFile.IsJS() {
			// JS types have a heap of constructs we can't directly emit into .d.ts files; the node builder contains logic to remap those where possible, so we invoke it here
			// In strada we always built js declarations symbolically, so all js type nodes went through this postprocessing
			jsFlags := declarationEmitNodeBuilderFlags
			if tx.inClassExpressionDeclaration {
				jsFlags &^= nodebuilder.FlagsWriteClassExpressionAsTypeLiteral
			}
			res := tx.resolver.TryJSTypeNodeToTypeNode(tx.EmitContext(), node.Type(), tx.enclosingDeclaration, jsFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
			if res != nil {
				return res
			}
			// otherwise, fall back to full serialization
		} else {
			return tx.Visitor().Visit(node.Type())
		}
	}

	oldErrorNameNode := tx.state.errorNameNode
	tx.state.errorNameNode = node.Name()
	var oldDiag GetSymbolAccessibilityDiagnostic
	if !tx.suppressNewDiagnosticContexts {
		oldDiag = tx.state.getSymbolAccessibilityDiagnostic
		if canProduceDiagnostics(node) {
			tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(node)
		}
	}
	var typeNode *ast.Node

	flags := declarationEmitNodeBuilderFlags
	if tx.inClassExpressionDeclaration {
		flags &^= nodebuilder.FlagsWriteClassExpressionAsTypeLiteral
	}
	if ast.HasInferredType(node) {
		typeNode = tx.resolver.CreateTypeOfDeclaration(tx.EmitContext(), node, tx.enclosingDeclaration, flags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	} else if ast.IsFunctionLike(node) {
		typeNode = tx.resolver.CreateReturnTypeOfSignatureDeclaration(tx.EmitContext(), node, tx.enclosingDeclaration, flags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	} else {
		debug.AssertNever(node)
	}

	tx.state.errorNameNode = oldErrorNameNode
	if !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	}
	if typeNode == nil {
		return tx.Factory().NewKeywordTypeNode(ast.KindAnyKeyword)
	}
	return typeNode
}

func (tx *DeclarationTransformer) shouldPrintWithInitializer(node *ast.Node) bool {
	return canHaveLiteralInitializer(tx.host, node) && node.Initializer() != nil && tx.resolver.IsLiteralConstDeclaration(tx.EmitContext().MostOriginal(node))
}

func (tx *DeclarationTransformer) checkEntityNameVisibility(entityName *ast.Node, enclosingDeclaration *ast.Node) {
	visibilityResult := tx.resolver.IsEntityNameVisible(entityName, enclosingDeclaration)
	tx.tracker.handleSymbolAccessibilityError(visibilityResult)
}

// Transforms the direct child of a source file into zero or more replacement statements
func (tx *DeclarationTransformer) transformTopLevelDeclaration(input *ast.Node) *ast.Node {
	if len(tx.state.lateMarkedStatements) > 0 {
		// Remove duplicates of the current statement from the deferred work queue (this was done via orderedRemoveItem in strada - why? to ensure the same backing array? microop?)
		tx.state.lateMarkedStatements = core.Filter(tx.state.lateMarkedStatements, func(node *ast.Node) bool { return node != input })
	}
	if tx.shouldStripInternal(input) {
		return nil
	}
	if input.Kind == ast.KindImportEqualsDeclaration {
		return tx.transformImportEqualsDeclaration(input.AsImportEqualsDeclaration())
	}
	if input.Kind == ast.KindImportDeclaration || input.Kind == ast.KindJSImportDeclaration {
		res := tx.transformImportDeclaration(input.AsImportDeclaration())
		if res != nil && res.Kind != ast.KindImportDeclaration {
			res := res.Clone(tx.Factory())
			res.Kind = ast.KindImportDeclaration
			return res
		}
		return res
	}
	if ast.IsDeclaration(input) && isDeclarationAndNotVisible(tx.EmitContext(), tx.resolver, input) {
		return nil
	}

	// !!! TODO: JSDoc support
	// if (isJSDocImportTag(input)) return;

	// Elide implementation signatures from overload sets
	if ast.IsFunctionLike(input) && tx.resolver.IsImplementationOfOverload(input) {
		return nil
	}
	original := tx.EmitContext().MostOriginal(input)
	id := ast.GetNodeId(original)
	if _, ok := tx.expandoHosts[id]; ok {
		return tx.createFullExpandoBlock(id)
	}

	previousEnclosingDeclaration := tx.enclosingDeclaration
	if isEnclosingDeclaration(input) {
		tx.enclosingDeclaration = input
	}

	canProdiceDiagnostic := canProduceDiagnostics(input)
	oldDiag := tx.state.getSymbolAccessibilityDiagnostic
	oldName := tx.state.errorNameNode
	if canProdiceDiagnostic {
		tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(input)
	}
	saveNeedsDeclare := tx.needsDeclare

	var result *ast.Node
	switch input.Kind {
	case ast.KindTypeAliasDeclaration, ast.KindJSTypeAliasDeclaration:
		result = tx.transformTypeAliasDeclaration(input.AsTypeAliasDeclaration())
	case ast.KindInterfaceDeclaration:
		result = tx.transformInterfaceDeclaration(input.AsInterfaceDeclaration())
	case ast.KindFunctionDeclaration:
		result = tx.transformFunctionDeclaration(input.AsFunctionDeclaration())
	case ast.KindModuleDeclaration:
		result = tx.transformModuleDeclaration(input.AsModuleDeclaration())
	case ast.KindClassDeclaration:
		result = tx.transformClassDeclaration(input.AsClassDeclaration())
	case ast.KindVariableStatement:
		result = tx.transformVariableStatement(input.AsVariableStatement())
	case ast.KindEnumDeclaration:
		result = tx.transformEnumDeclaration(input.AsEnumDeclaration())
	default:
		// Anything left unhandled is an error, so this should be unreachable
		panic(fmt.Sprintf("Unhandled top-level node in declaration emit: %q", input.Kind))
	}

	tx.enclosingDeclaration = previousEnclosingDeclaration
	tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	tx.needsDeclare = saveNeedsDeclare
	tx.state.errorNameNode = oldName
	return result
}

func (tx *DeclarationTransformer) transformTypeAliasDeclaration(input *ast.TypeAliasDeclaration) *ast.Node {
	tx.needsDeclare = false
	return tx.Factory().UpdateTypeAliasDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		tx.Visitor().VisitNodes(input.TypeParameters),
		tx.Visitor().Visit(input.Type),
	)
}

func (tx *DeclarationTransformer) transformInterfaceDeclaration(input *ast.InterfaceDeclaration) *ast.Node {
	return tx.Factory().UpdateInterfaceDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		tx.Visitor().VisitNodes(input.TypeParameters),
		tx.Visitor().VisitNodes(input.HeritageClauses),
		tx.Visitor().VisitNodes(input.Members),
	)
}

func (tx *DeclarationTransformer) transformFunctionDeclaration(input *ast.FunctionDeclaration) *ast.Node {
	if tx.resolver.IsExpandoFunctionDeclaration(input.AsNode()) {
		tx.state.reportExpandoFunctionErrors(input.AsNode())
	}
	return tx.Factory().UpdateFunctionDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		nil,
		input.Name(),
		tx.ensureTypeParams(input.AsNode(), input.TypeParameters),
		tx.updateParamList(input.AsNode(), input.Parameters),
		tx.ensureType(input.AsNode(), false),
		nil, /*fullSignature*/
		nil,
	)
}

func (tx *DeclarationTransformer) transformModuleDeclaration(input *ast.ModuleDeclaration) *ast.Node {
	// !!! TODO: module declarations are now parsed into nested module objects with export modifiers
	// It'd be good to collapse those back in the declaration output, but the AST can't represent the
	// `namespace a.b.c` shape for the printer (without using invalid identifier names).
	mods := tx.ensureModifiers(input.AsNode())
	saveNeedsDeclare := tx.needsDeclare
	tx.needsDeclare = false
	inner := input.Body
	keyword := input.Keyword
	if keyword != ast.KindGlobalKeyword && (input.Name() == nil || !ast.IsStringLiteral(input.Name())) {
		keyword = ast.KindNamespaceKeyword
	}

	if inner != nil && inner.Kind == ast.KindModuleBlock {
		oldNeedsScopeFix := tx.needsScopeFixMarker
		oldHasScopeFix := tx.resultHasScopeMarker
		tx.resultHasScopeMarker = false
		tx.needsScopeFixMarker = false
		statements := tx.Visitor().VisitNodes(inner.StatementList())
		lateStatements := tx.transformAndReplaceLatePaintedStatements(statements)
		if input.Flags&ast.NodeFlagsAmbient != 0 {
			tx.needsScopeFixMarker = false // If it was `declare`'d everything is implicitly exported already, ignore late printed "privates"
		}
		// With the final list of statements, there are 3 possibilities:
		// 1. There's an export assignment or export declaration in the namespace - do nothing
		// 2. Everything is exported and there are no export assignments or export declarations - strip all export modifiers
		// 3. Some things are exported, some are not, and there's no marker - add an empty marker
		if !ast.IsGlobalScopeAugmentation(input.AsNode()) && !tx.resultHasScopeMarker && !hasScopeMarker(lateStatements) {
			if tx.needsScopeFixMarker {
				lateStatements = tx.Factory().NewNodeList(append(lateStatements.Nodes, createEmptyExports(tx.Factory().AsNodeFactory())))
			} else {
				lateStatements = tx.exportStrippingVisitor.VisitNodes(lateStatements)
			}
		}

		body := tx.Factory().UpdateModuleBlock(inner.AsModuleBlock(), lateStatements)
		tx.needsDeclare = saveNeedsDeclare
		tx.needsScopeFixMarker = oldNeedsScopeFix
		tx.resultHasScopeMarker = oldHasScopeFix

		return tx.Factory().UpdateModuleDeclaration(
			input,
			mods,
			keyword,
			input.Name(),
			body,
		)
	}
	if inner != nil {
		// trigger visit. ignore result (is deferred, so is just inner unless elided)
		tx.Visitor().Visit(inner)
		// eagerly transform nested namespaces (the nesting doesn't need any elision or painting done)
		original := tx.EmitContext().MostOriginal(inner)
		id := ast.GetNodeId(original)
		body, _ := tx.lateStatementReplacementMap[id]
		delete(tx.lateStatementReplacementMap, id)
		return tx.Factory().UpdateModuleDeclaration(
			input,
			mods,
			keyword,
			input.Name(),
			body,
		)
	}
	return tx.Factory().UpdateModuleDeclaration(
		input,
		mods,
		keyword,
		input.Name(),
		nil,
	)
}

func (tx *DeclarationTransformer) stripExportModifiers(statement *ast.Node) *ast.Node {
	if statement == nil {
		return nil
	}
	parseNode := tx.EmitContext().ParseNode(statement)
	if ast.IsImportEqualsDeclaration(statement) || (parseNode != nil && tx.host.GetEffectiveDeclarationFlags(parseNode, ast.ModifierFlagsDefault) != 0) || !ast.CanHaveModifiers(statement) {
		// `export import` statements should remain as-is, as imports are _not_ implicitly exported in an ambient namespace
		// Likewise, `export default` classes and the like and just be `default`, so we preserve their `export` modifiers, too
		return statement
	}

	oldFlags := ast.GetCombinedModifierFlags(statement)
	if oldFlags&ast.ModifierFlagsExport == 0 {
		return statement
	}
	newFlags := oldFlags & (ast.ModifierFlagsAll ^ ast.ModifierFlagsExport)
	modifiers := ast.CreateModifiersFromModifierFlags(newFlags, tx.Factory().NewModifier)
	return ast.ReplaceModifiers(tx.Factory().AsNodeFactory(), statement, tx.Factory().NewModifierList(modifiers))
}

// buildClassMembers builds the member list for a class-like node (ClassDeclaration or ClassExpression).
// It handles parameter properties, private identifiers, late-bound index signatures, and visited members.
// Extra members (e.g., this-property assignments from JS files) can be passed via extraMembers.
func (tx *DeclarationTransformer) buildClassMembers(classNode *ast.Node, extraMembers ...*ast.Node) *ast.NodeList {
	ctor := ast.GetFirstConstructorWithBody(classNode)
	var parameterProperties []*ast.Node
	if ctor != nil {
		oldDiag := tx.state.getSymbolAccessibilityDiagnostic
		for _, param := range ctor.AsConstructorDeclaration().Parameters.Nodes {
			if !ast.HasSyntacticModifier(param, ast.ModifierFlagsParameterPropertyModifier) || tx.shouldStripInternal(param) {
				continue
			}
			tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(param)
			if param.Name().Kind == ast.KindIdentifier {
				updated := tx.Factory().NewPropertyDeclaration(
					tx.ensureModifiers(param),
					param.Name(),
					param.QuestionToken(),
					tx.ensureType(param, false),
					tx.ensureNoInitializer(param),
				)
				tx.preserveJsDoc(updated, param)
				parameterProperties = append(parameterProperties, updated)
			} else {
				// Pattern - this is currently an error, but we emit declarations for it somewhat correctly
				parameterProperties = append(parameterProperties, tx.walkBindingPattern(param.Name().AsBindingPattern(), param)...)
			}
		}
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	}

	// When the class has at least one private identifier, create a unique constant identifier to retain the nominal typing behavior
	// Prevents other classes with the same public members from being used in place of the current class
	var privateIdentifier *ast.Node
	if core.Some(classNode.ClassLikeData().Members.Nodes, func(member *ast.Node) bool {
		return member.Name() != nil && ast.IsPrivateIdentifier(member.Name())
	}) {
		privateIdentifier = tx.Factory().NewPropertyDeclaration(nil, tx.Factory().NewPrivateIdentifier("#private"), nil, nil, nil)
	}

	lateIndexes := tx.resolver.CreateLateBoundIndexSignatures(
		tx.EmitContext(),
		classNode,
		tx.enclosingDeclaration,
		declarationEmitNodeBuilderFlags,
		declarationEmitInternalNodeBuilderFlags,
		tx.tracker,
	)

	memberNodes := make([]*ast.Node, 0, len(classNode.ClassLikeData().Members.Nodes))
	if privateIdentifier != nil {
		memberNodes = append(memberNodes, privateIdentifier)
	}
	memberNodes = append(memberNodes, lateIndexes...)
	memberNodes = append(memberNodes, parameterProperties...)
	memberNodes = append(memberNodes, extraMembers...)
	visitResult := tx.Visitor().VisitNodes(classNode.ClassLikeData().Members)
	if visitResult != nil && len(visitResult.Nodes) > 0 {
		memberNodes = append(memberNodes, visitResult.Nodes...)
	}
	return tx.Factory().NewNodeList(memberNodes)
}

func (tx *DeclarationTransformer) transformClassDeclaration(input *ast.ClassDeclaration) *ast.Node {
	previousEnclosingDeclaration := tx.enclosingDeclaration
	tx.enclosingDeclaration = input.AsNode()
	defer func() { tx.enclosingDeclaration = previousEnclosingDeclaration }()

	tx.state.errorNameNode = input.Name()
	tx.tracker.PushErrorFallbackNode(input.AsNode())
	defer tx.tracker.PopErrorFallbackNode()

	modifiers := tx.ensureModifiers(input.AsNode())
	typeParameters := tx.ensureTypeParams(input.AsNode(), input.TypeParameters)

	// Collect this.x property assignments from constructors and static blocks in JS files
	var extraMembers []*ast.Node
	if ast.IsInJSFile(input.AsNode()) {
		extraMembers = tx.collectThisPropertyAssignments(input.AsNode())
	}

	members := tx.buildClassMembers(input.AsNode(), extraMembers...)

	extendsClause := getEffectiveBaseTypeNode(input.AsNode())

	if extendsClause != nil && !ast.IsEntityNameExpression(extendsClause.AsExpressionWithTypeArguments().Expression) && extendsClause.AsExpressionWithTypeArguments().Expression.Kind != ast.KindNullKeyword {
		tx.tracker.ReportInferenceFallback(extendsClause.AsExpressionWithTypeArguments().Expression) // Add an isolated declarations error on this extends clause
		oldId := "default"
		if ast.NodeIsPresent(input.Name()) && ast.IsIdentifier(input.Name()) && len(input.Name().Text()) > 0 {
			oldId = input.Name().Text()
		}
		newId := tx.Factory().NewUniqueNameEx(oldId+"_base", printer.AutoGenerateOptions{Flags: printer.GeneratedIdentifierFlagsOptimistic})
		tx.state.getSymbolAccessibilityDiagnostic = func(_ printer.SymbolAccessibilityResult) *SymbolAccessibilityDiagnostic {
			return &SymbolAccessibilityDiagnostic{
				diagnosticMessage: diagnostics.X_extends_clause_of_exported_class_0_has_or_is_using_private_name_1,
				errorNode:         extendsClause,
				typeName:          input.Name(),
			}
		}

		varDecl := tx.Factory().NewVariableDeclaration(
			newId,
			nil,
			tx.resolver.CreateTypeOfExpression(tx.EmitContext(), extendsClause.Expression(), input.AsNode(), declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker),
			nil,
		)
		var mods *ast.ModifierList
		if tx.needsDeclare {
			mods = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindDeclareKeyword)})
		}
		statement := tx.Factory().NewVariableStatement(
			mods,
			tx.Factory().NewVariableDeclarationList(tx.Factory().NewNodeList([]*ast.Node{varDecl}), ast.NodeFlagsConst),
		)
		newHeritageClause := tx.Factory().UpdateHeritageClause(
			extendsClause.Parent.AsHeritageClause(),
			extendsClause.Parent.AsHeritageClause().Token,
			tx.Factory().NewNodeList([]*ast.Node{
				tx.Factory().UpdateExpressionWithTypeArguments(
					extendsClause.AsExpressionWithTypeArguments(),
					newId,
					tx.Visitor().VisitNodes(extendsClause.AsExpressionWithTypeArguments().TypeArguments),
				),
			}),
		)
		retainedHeritageClauses := tx.Visitor().VisitNodes(input.HeritageClauses) // should just be `implements`
		heritageList := []*ast.Node{
			newHeritageClause,
		}
		if retainedHeritageClauses != nil && len(retainedHeritageClauses.Nodes) > 0 {
			heritageList = append(heritageList, retainedHeritageClauses.Nodes...)
		}
		heritageClauses := tx.Factory().NewNodeList(heritageList)

		return tx.Factory().NewSyntaxList([]*ast.Node{
			statement,
			tx.Factory().UpdateClassDeclaration(
				input,
				modifiers,
				input.Name(),
				typeParameters,
				heritageClauses,
				members,
			),
		})
	}

	return tx.Factory().UpdateClassDeclaration(
		input,
		modifiers,
		input.Name(),
		typeParameters,
		tx.Visitor().VisitNodes(input.HeritageClauses),
		members,
	)
}

func (tx *DeclarationTransformer) visitThisPropertyAssignments(node *ast.Node) *ast.Node {
	var thisTarget *ast.Node
	isStatic := false
	thisContainer := ast.GetThisContainer(node, false, false)
	thisTarget = thisContainer.Parent
	if thisTarget == nil {
		return nil // thisContainer was source file, can't have expando-this
	}
	if ast.HasStaticModifier(thisContainer) || ast.IsClassStaticBlockDeclaration(thisContainer) {
		isStatic = true
	}
	if thisTarget != tx.enclosingDeclaration {
		return nil // stop searching within new `this` contexts
	}
caseBlock:
	switch ast.GetAssignmentDeclarationKind(node) {
	case ast.JSDeclarationKindThisProperty:
		name := ast.GetNameOfDeclaration(node)
		base := tx.resolver.GetReferencedMemberValueDeclaration(node)
		key := getThisPropertyAssignmentKey(name, node, isStatic)
		if base == nil || tx.seenProperties.Has(key) {
			break
		}
		tx.seenProperties.Add(key)

		// problem: this prop might be overriding a prop from a base type. The checker has special bails for override compat comparisons for binary expression properties,
		// but what we transform to won't - so we either need to match the base type (for example, if it's a getter/setter) or emit nothing
		// See `checkKindsOfPropertyMemberOverrides` in the checker for what we're trying to satisfy here
		if thisTarget.ClassLikeData().HeritageClauses != nil && len(thisTarget.ClassLikeData().HeritageClauses.Nodes) > 0 && !isClassExtendingNull(thisTarget) {
			// there is a base type any assignments might be "from"
			tx.tracker.ReportInferenceFallback(thisTarget) // Add an isolated declarations error on this class - we can't know how to transform this prop into an assignment without referring to type information
			if tx.resolver.IsThisPropertyAssignmentDeclarationRedundant(node) {
				break caseBlock // skip assignments whose member is already provided by an `extends` base type (an inherited accessor/method, or an identical inherited property)
				// TODO: If the property has an explicit `@type` annotation, we should probably emit it (maybe with an `override` modifier) instead of skipping it
			}
		}

		var mods *ast.ModifierList
		if isStatic {
			mods = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindStaticKeyword)})
		}
		if ast.HasDynamicName(node) {
			if !transformers.IsSimpleInlineableExpression(name) {
				break // Member either becomes an index signature or is a reassignment
			}
			tx.checkName(node)
			name = tx.Factory().NewComputedPropertyName(name) // Convert `this[foo] = expr` to `[foo]: Type`
		}
		if ast.GetTextOfPropertyName(name) == "constructor" {
			break // `constructor` is a builtin class member, not allowed to redeclare it
		}
		if ast.IsIdentifier(name) && !scanner.IsIdentifierText(name.Text(), core.LanguageVariantStandard) {
			name = tx.Factory().NewStringLiteralFromNode(name)
		}
		prop := tx.Factory().NewPropertyDeclaration(
			mods,
			name,
			nil,
			tx.ensureType(node, false),
			nil,
		)
		if ast.IsExpressionStatement(node.Parent) {
			tx.preserveJsDoc(prop, node.Parent)
		}
		tx.thisPropertyAssignmentsCollected = append(tx.thisPropertyAssignmentsCollected, prop)
	}
	return tx.thisPropertyVisitor.VisitEachChild(node)
}

func isClassExtendingNull(node *ast.Node) bool {
	if node == nil {
		return false
	}
	heritage := node.ClassLikeData().HeritageClauses
	if heritage == nil {
		return false
	}
	if len(heritage.Nodes) > 1 || len(heritage.Nodes) == 0 {
		return false
	}
	for _, expA := range heritage.Nodes[0].AsHeritageClause().Types.Nodes {
		expr := expA.AsExpressionWithTypeArguments().Expression
		if expr != nil && expr.Kind == ast.KindNullKeyword {
			return true
		}
	}
	return false
}

// collectThisPropertyAssignments finds `this.x = expr` assignments in constructors, methods, and static blocks
// of JS classes and synthesizes PropertyDeclaration nodes for each unique property name.
func (tx *DeclarationTransformer) collectThisPropertyAssignments(classNode *ast.Node) []*ast.Node {
	members := classNode.ClassLikeData().Members
	seen := collections.Set[thisPropertyAssignmentKey]{}
	// Pre-populate seen with existing direct member nodes to avoid duplicates
	for _, member := range members.Nodes {
		if member.Name() != nil {
			isStatic := ast.IsStatic(member)
			seen.Add(getThisPropertyAssignmentKey(member.Name(), member, isStatic))
		}
	}
	tx.seenProperties = seen
	defer tx.seenProperties.Clear()
	tx.thisPropertyAssignmentsCollected = []*ast.Node{}
	defer func() {
		tx.thisPropertyAssignmentsCollected = nil
	}()

	for _, n := range members.Nodes {
		tx.thisPropertyVisitor.VisitEachChild(n)
	}
	return tx.thisPropertyAssignmentsCollected
}

func (tx *DeclarationTransformer) walkBindingPattern(pattern *ast.BindingPattern, param *ast.Node) []*ast.Node {
	var elems []*ast.Node
	for _, elem := range pattern.Elements.Nodes {
		if ast.IsOmittedExpression(elem) {
			continue
		}
		if ast.IsBindingPattern(elem.Name()) {
			elems = append(elems, tx.walkBindingPattern(elem.Name().AsBindingPattern(), param)...)
			continue
		}
		elems = append(elems, tx.Factory().NewPropertyDeclaration(
			tx.ensureModifiers(param),
			elem.Name(),
			nil, /*questionOrExclamationToken*/
			tx.ensureType(elem, false),
			nil, /*initializer*/
		))
	}
	return elems
}

func (tx *DeclarationTransformer) transformVariableStatement(input *ast.VariableStatement) *ast.Node {
	visible := false
	for _, decl := range input.DeclarationList.AsVariableDeclarationList().Declarations.Nodes {
		visible = getBindingNameVisible(tx.resolver, decl)
		if visible {
			break
		}
	}
	if !visible {
		return nil
	}

	inputNodes := input.DeclarationList.AsVariableDeclarationList().Declarations.Nodes
	var extraImports []*ast.Node
	if tx.state.currentSourceFile.CommonJSModuleIndicator != nil {
		var normalDeclarations []*ast.Node
		var imports []*ast.Node
		for _, n := range inputNodes {
			if ast.IsVariableDeclarationInitializedToRequire(n) {
				imports = append(imports, n)
			} else {
				normalDeclarations = append(normalDeclarations, n)
			}
		}
		inputNodes = normalDeclarations
		extraImports, _ = tx.Visitor().VisitSlice(imports)
	}

	nodes, _ := tx.Visitor().VisitSlice(inputNodes)
	if len(nodes) == 0 {
		if len(extraImports) > 0 {
			return tx.Factory().NewSyntaxList(extraImports)
		}
		return nil
	}
	nodeList := tx.Factory().NewNodeList(nodes)

	modifiers := tx.ensureModifiers(input.AsNode())

	var declList *ast.Node
	if ast.IsVarUsing(input.DeclarationList) || ast.IsVarAwaitUsing(input.DeclarationList) {
		declList = tx.Factory().NewVariableDeclarationList(nodeList, ast.NodeFlagsConst)
		tx.EmitContext().SetOriginal(declList, input.DeclarationList)
		tx.EmitContext().SetCommentRange(declList, input.DeclarationList.Loc)
		declList.Loc = input.DeclarationList.Loc
	} else {
		declList = tx.Factory().UpdateVariableDeclarationList(input.DeclarationList.AsVariableDeclarationList(), nodeList, input.DeclarationList.Flags)
	}
	res := tx.Factory().UpdateVariableStatement(input, modifiers, declList)
	if len(extraImports) > 0 {
		return tx.Factory().NewSyntaxList(append(extraImports, res))
	}
	return res
}

func (tx *DeclarationTransformer) transformEnumDeclaration(input *ast.EnumDeclaration) *ast.Node {
	return tx.Factory().UpdateEnumDeclaration(
		input,
		tx.ensureModifiers(input.AsNode()),
		input.Name(),
		tx.Factory().NewNodeList(core.MapNonNil(input.Members.Nodes, func(m *ast.Node) *ast.Node {
			if tx.shouldStripInternal(m) {
				return nil
			}

			// Rewrite enum values to their constants, if available
			enumValue := tx.resolver.GetEnumMemberValue(m)

			if tx.state.isolatedDeclarations && m.Initializer() != nil && enumValue.HasExternalReferences &&
				// This will be its own compiler error instead, so don't report.
				!ast.IsComputedPropertyName(m.Name()) {
				tx.state.addDiagnostic(createDiagnosticForNode(m, diagnostics.Enum_member_initializers_must_be_computable_without_references_to_external_symbols_with_isolatedDeclarations))
			}

			var newInitializer *ast.Node
			switch value := enumValue.Value.(type) {
			case jsnum.Number:
				if value.IsInf() {
					if value > 0 {
						newInitializer = tx.Factory().NewIdentifier("Infinity")
					} else {
						newInitializer = tx.Factory().NewPrefixUnaryExpression(ast.KindMinusToken, tx.Factory().NewIdentifier("Infinity"))
					}
				} else if value.IsNaN() {
					newInitializer = tx.Factory().NewIdentifier("NaN")
				} else if value >= 0 {
					newInitializer = tx.Factory().NewNumericLiteral(value.String(), ast.TokenFlagsNone)
				} else {
					newInitializer = tx.Factory().NewPrefixUnaryExpression(
						ast.KindMinusToken,
						tx.Factory().NewNumericLiteral((-value).String(), ast.TokenFlagsNone),
					)
				}
			case string:
				newInitializer = tx.Factory().NewStringLiteral(value, ast.TokenFlagsNone)
			default:
				// nil
				newInitializer = nil
			}
			result := tx.Factory().UpdateEnumMember(m.AsEnumMember(), m.Name(), newInitializer)
			tx.preserveJsDoc(result, m)
			return result
		})),
	)
}

type effectSchemaRequestBaseInfo struct {
	modelName string
	brand     *ast.Node
}

func (tx *DeclarationTransformer) createEffectSchemaSourceFileDeclarations(statements *ast.StatementList) *ast.StatementList {
	if statements == nil || tx.state.currentSourceFile == nil {
		return statements
	}

	modelNames := map[string]bool{}
	existingNamespaces := map[string]bool{}
	classes := map[string]*ast.Node{}
	schemaClasses := tx.getEffectSchemaOriginalClasses()

	for _, statement := range statements.Nodes {
		if className := getEffectSchemaClassName(statement); className != "" && ast.IsClassDeclaration(statement) {
			classes[className] = statement
		}
	}

	for _, statement := range statements.Nodes {
		if isEffectSchemaModelNamespace(statement) || isEffectSchemaMaterializedModelNamespace(statement, classes) {
			name := moduleDeclarationIdentifierName(statement)
			if name != "" && schemaClasses[name] != nil {
				modelNames[name] = true
				existingNamespaces[name] = true
			}
		}
	}

	requestBaseInfos := map[string]effectSchemaRequestBaseInfo{}
	for _, statement := range statements.Nodes {
		if info, ok := getEffectSchemaRequestBaseInfo(statement); ok && classes[info.modelName] != nil && schemaClasses[info.modelName] == nil {
			requestBaseInfos[info.modelName] = info
		}
	}

	for className, classDeclaration := range schemaClasses {
		if !existingNamespaces[className] && tx.canCreateEffectSchemaGeneratedNamespace(classDeclaration) {
			modelNames[className] = true
		}
	}

	// Top-level `const X = S.Struct(...)` / `S.TaggedStruct(...)` schema values. Faceted on
	// the const itself (it is a value, not a class): the giant `S.Struct<{...}>` annotation
	// becomes a compact `StructFacade<...>` plus a generated `interface X` (decoded Self) and
	// a type-only `declare namespace X`.
	structModelNames := map[string]bool{}
	for name := range tx.getEffectSchemaOriginalStructs() {
		if modelNames[name] {
			continue
		}
		if _, ok := requestBaseInfos[name]; ok {
			continue
		}
		if hasTopLevelInterface(statements, name) || hasTopLevelNamespace(statements, name) {
			continue
		}
		if tx.canCreateEffectSchemaGeneratedStructNamespace(name) {
			structModelNames[name] = true
		}
	}

	if len(modelNames) == 0 && len(requestBaseInfos) == 0 && len(structModelNames) == 0 {
		return statements
	}

	changed := false
	next := make([]*ast.Node, 0, len(statements.Nodes))
	for _, statement := range statements.Nodes {
		baseModelName := getEffectSchemaBaseModelName(statement)
		if baseModelName != "" && modelNames[baseModelName] {
			classDeclaration := schemaClasses[baseModelName]
			if classDeclaration == nil {
				classDeclaration = classes[baseModelName]
			}
			if classDeclaration != nil {
				if updated := tx.updateEffectSchemaBaseDeclaration(statement, baseModelName, classDeclaration, needsEffectSchemaIntermediateClass(classDeclaration)); updated != nil {
					changed = true
					next = append(next, updated)
					continue
				}
			}
		}

		if requestBaseInfo, ok := requestBaseInfos[baseModelName]; ok {
			if updated := tx.updateEffectSchemaRequestBaseDeclaration(statement, requestBaseInfo); updated != nil {
				changed = true
				next = append(next, updated)
				continue
			}
		}

		if isEffectSchemaModelNamespace(statement) || isEffectSchemaMaterializedModelNamespace(statement, classes) {
			name := moduleDeclarationIdentifierName(statement)
			classDeclaration := schemaClasses[name]
			if classDeclaration == nil {
				classDeclaration = classes[name]
			}
			if classDeclaration != nil {
				if updated := tx.updateEffectSchemaNamespaceDeclaration(statement, classDeclaration); updated != nil {
					changed = true
					next = append(next, updated)
					continue
				}
			}
		}

		className := getEffectSchemaClassName(statement)
		if requestBaseInfos[className].modelName != "" {
			classDeclaration := statement
			var typeInterface *ast.Node
			if !hasTopLevelInterface(statements, className) {
				typeInterface = tx.createEffectSchemaTypeInterface(classDeclaration)
			}
			var namespace *ast.Node
			if !hasTopLevelNamespace(statements, className) {
				namespace = tx.createEffectSchemaGeneratedNamespaceDeclaration(className, classDeclaration)
			}
			if typeInterface != nil || namespace != nil {
				changed = true
				next = append(next, statement)
				if typeInterface != nil {
					next = append(next, typeInterface)
				}
				if namespace != nil {
					next = append(next, namespace)
				}
				continue
			}
		}

		if className != "" && modelNames[className] {
			classDeclaration := schemaClasses[className]
			if classDeclaration == nil {
				classDeclaration = statement
			}
			usesIntermediate := needsEffectSchemaIntermediateClass(classDeclaration)
			updatedClass := statement
			if usesIntermediate {
				updatedClass = tx.updateEffectSchemaClassDeclaration(statement, className)
			}
			var typeInterface *ast.Node
			if !hasTopLevelInterface(statements, className) {
				typeInterface = tx.createEffectSchemaTypeInterface(classDeclaration)
			}
			var namespace *ast.Node
			if !existingNamespaces[className] {
				namespace = tx.createEffectSchemaGeneratedNamespaceDeclaration(className, classDeclaration)
			}
			if updatedClass != statement || typeInterface != nil || namespace != nil {
				changed = true
				if usesIntermediate {
					next = append(next, tx.createEffectSchemaIntermediateClass(className))
				}
				next = append(next, updatedClass)
				if typeInterface != nil {
					next = append(next, typeInterface)
				}
				if namespace != nil {
					existingNamespaces[className] = true
					next = append(next, namespace)
				}
				continue
			}
		}

		if structName := getEffectSchemaStructVariableName(statement); structName != "" && structModelNames[structName] {
			if declarations := tx.createEffectSchemaStructDeclarations(statement, structName); declarations != nil {
				changed = true
				next = append(next, declarations...)
				continue
			}
		}

		if isEffectSchemaStructCompanionTypeAlias(statement, structModelNames) {
			// Dropped — replaced by the generated `interface X`.
			changed = true
			continue
		}

		next = append(next, statement)
	}

	if !changed {
		return statements
	}
	return tx.Factory().NewNodeList(next)
}

func (tx *DeclarationTransformer) getEffectSchemaOriginalClasses() map[string]*ast.Node {
	classes := map[string]*ast.Node{}
	for _, statement := range tx.state.currentSourceFile.Statements.Nodes {
		if ast.IsClassDeclaration(statement) && statement.Name() != nil && hasEffectSchemaOpaqueHeritage(statement) {
			classes[statement.Name().Text()] = statement
		}
	}
	return classes
}

// --- Struct/TaggedStruct const faceting ---

func (tx *DeclarationTransformer) getEffectSchemaOriginalStructs() map[string]bool {
	structs := map[string]bool{}
	for _, statement := range tx.state.currentSourceFile.Statements.Nodes {
		name := getEffectSchemaStructVariableName(statement)
		if name == "" {
			continue
		}
		decl := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes[0]
		if decl.AsVariableDeclaration().Initializer != nil && isEffectSchemaStructInitializer(decl.AsVariableDeclaration().Initializer) {
			structs[name] = true
		}
	}
	return structs
}

func isEffectSchemaStructInitializer(expression *ast.Node) bool {
	if !ast.IsCallExpression(expression) {
		return false
	}
	callee := expression.AsCallExpression().Expression
	if !ast.IsPropertyAccessExpression(callee) || callee.Name() == nil {
		return false
	}
	name := callee.Name().Text()
	if name != "Struct" && name != "TaggedStruct" {
		return false
	}
	left := callee.Expression()
	return left != nil && ast.IsIdentifier(left) && (left.Text() == "S" || left.Text() == "Schema")
}

func getEffectSchemaStructVariableName(statement *ast.Node) string {
	if !ast.IsVariableStatement(statement) || statement.AsVariableStatement().DeclarationList == nil {
		return ""
	}
	declarations := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes
	if len(declarations) != 1 || declarations[0].Name() == nil || !ast.IsIdentifier(declarations[0].Name()) {
		return ""
	}
	return declarations[0].Name().Text()
}

func (tx *DeclarationTransformer) getEffectSchemaSourceStructDeclaration(modelName string) *ast.Node {
	for _, statement := range tx.state.currentSourceFile.Statements.Nodes {
		if getEffectSchemaStructVariableName(statement) != modelName {
			continue
		}
		decl := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes[0]
		if decl.AsVariableDeclaration().Initializer != nil && isEffectSchemaStructInitializer(decl.AsVariableDeclaration().Initializer) {
			return decl
		}
	}
	return nil
}

// Reads a property (Encoded / Type / ~type.make.in / fields / services) off the source
// struct value's type and serializes it; `never` services stay `never`.
func (tx *DeclarationTransformer) materializeEffectSchemaStructProperty(modelName string, propertyName string) *ast.Node {
	declaration := tx.getEffectSchemaSourceStructDeclaration(modelName)
	if declaration == nil {
		return nil
	}
	return tx.resolver.CreateTypeOfStructSchemaProperty(tx.EmitContext(), declaration, propertyName, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
}

func (tx *DeclarationTransformer) canCreateEffectSchemaGeneratedStructNamespace(modelName string) bool {
	return tx.materializeEffectSchemaStructProperty(modelName, "Encoded") != nil
}

func (tx *DeclarationTransformer) createEffectSchemaStructInterfaceFromProperty(modelName string, propertyName string, declaredName string) *ast.Node {
	typeNode := tx.materializeEffectSchemaStructProperty(modelName, propertyName)
	if typeNode == nil {
		return nil
	}
	if ast.IsTypeLiteralNode(typeNode) {
		return tx.Factory().NewInterfaceDeclaration(nil, tx.Factory().NewIdentifier(declaredName), nil, nil, tx.Factory().NewNodeList(typeNode.AsTypeLiteralNode().Members.Nodes))
	}
	return tx.Factory().NewTypeAliasDeclaration(nil, tx.Factory().NewIdentifier(declaredName), nil, typeNode)
}

func (tx *DeclarationTransformer) createEffectSchemaStructServiceDeclaration(modelName string, name string) *ast.Node {
	resolved := tx.materializeEffectSchemaStructProperty(modelName, name)
	serviceType := resolved
	if resolved == nil || resolved.Kind == ast.KindAnyKeyword {
		serviceType = tx.Factory().NewKeywordTypeNode(ast.KindNeverKeyword)
	}
	return tx.Factory().NewTypeAliasDeclaration(nil, tx.Factory().NewIdentifier(name), nil, serviceType)
}

func structHasExportModifier(statement *ast.Node) bool {
	return ast.HasSyntacticModifier(statement, ast.ModifierFlagsExport)
}

func (tx *DeclarationTransformer) effectSchemaStructModifiers(exported bool, includeDeclare bool) *ast.ModifierList {
	modifiers := []*ast.Node{}
	if exported {
		modifiers = append(modifiers, tx.Factory().NewModifier(ast.KindExportKeyword))
	}
	if includeDeclare {
		modifiers = append(modifiers, tx.Factory().NewModifier(ast.KindDeclareKeyword))
	}
	if len(modifiers) == 0 {
		return nil
	}
	return tx.Factory().NewModifierList(modifiers)
}

func (tx *DeclarationTransformer) createEffectSchemaGeneratedStructNamespace(modelName string, exported bool) *ast.Node {
	fields := tx.createEffectSchemaStructInterfaceFromProperty(modelName, "fields", "Fields")
	if fields == nil {
		return nil
	}
	encoded := tx.createEffectSchemaStructInterfaceFromProperty(modelName, "Encoded", "Encoded")
	if encoded == nil {
		return nil
	}
	statements := []*ast.Node{fields, encoded}
	if makeDeclaration := tx.createEffectSchemaStructInterfaceFromProperty(modelName, "~type.make.in", "Make"); makeDeclaration != nil {
		statements = append(statements, makeDeclaration)
	}
	statements = append(statements, tx.createEffectSchemaStructServiceDeclaration(modelName, "DecodingServices"))
	statements = append(statements, tx.createEffectSchemaStructServiceDeclaration(modelName, "EncodingServices"))
	return tx.Factory().NewModuleDeclaration(
		tx.effectSchemaStructModifiers(exported, true),
		ast.KindNamespaceKeyword,
		tx.Factory().NewIdentifier(modelName),
		tx.Factory().NewModuleBlock(tx.Factory().NewNodeList(statements)),
	)
}

// `import("#lib/StructFacade").StructFacade<X, X.Encoded, X.Make, X.DecodingServices,
// X.EncodingServices, X.Fields>` — a self-contained import type resolved cross-package via
// the api package's `#lib/*` subpath import. The scanner-local facade extends
// `S.Struct<Fields>`, so the value stays Workflow-compatible.
func (tx *DeclarationTransformer) createEffectSchemaStructFacadeType(modelName string) *ast.Node {
	member := func(name string) *ast.Node {
		return tx.Factory().NewTypeReferenceNode(tx.Factory().NewQualifiedName(tx.Factory().NewIdentifier(modelName), tx.Factory().NewIdentifier(name)), nil)
	}
	typeArguments := tx.Factory().NewNodeList([]*ast.Node{
		tx.Factory().NewTypeReferenceNode(tx.Factory().NewIdentifier(modelName), nil),
		member("Encoded"),
		member("Make"),
		member("DecodingServices"),
		member("EncodingServices"),
		member("Fields"),
	})
	argument := tx.Factory().NewLiteralTypeNode(tx.Factory().NewStringLiteral("#lib/StructFacade", ast.TokenFlagsNone))
	return tx.Factory().NewImportTypeNode(false, argument, nil, tx.Factory().NewIdentifier("StructFacade"), typeArguments)
}

func (tx *DeclarationTransformer) createEffectSchemaStructDeclarations(statement *ast.Node, modelName string) []*ast.Node {
	typeNode := tx.materializeEffectSchemaStructProperty(modelName, "Type")
	if typeNode == nil || !ast.IsTypeLiteralNode(typeNode) {
		return nil
	}
	namespace := tx.createEffectSchemaGeneratedStructNamespace(modelName, structHasExportModifier(statement))
	if namespace == nil {
		return nil
	}
	exported := structHasExportModifier(statement)
	declaration := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes[0]
	updatedDeclaration := tx.Factory().UpdateVariableDeclaration(
		declaration.AsVariableDeclaration(),
		declaration.Name(),
		declaration.AsVariableDeclaration().ExclamationToken,
		tx.createEffectSchemaStructFacadeType(modelName),
		declaration.AsVariableDeclaration().Initializer,
	)
	declarations := tx.Factory().NewNodeList([]*ast.Node{updatedDeclaration})
	declarationList := tx.Factory().UpdateVariableDeclarationList(statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList(), declarations, statement.AsVariableStatement().DeclarationList.Flags)
	retypedConst := tx.Factory().UpdateVariableStatement(statement.AsVariableStatement(), statement.Modifiers(), declarationList)
	typeInterface := tx.Factory().NewInterfaceDeclaration(tx.effectSchemaStructModifiers(exported, false), tx.Factory().NewIdentifier(modelName), nil, nil, tx.Factory().NewNodeList(typeNode.AsTypeLiteralNode().Members.Nodes))
	return []*ast.Node{retypedConst, typeInterface, namespace}
}

func isEffectSchemaStructCompanionTypeAlias(statement *ast.Node, structModelNames map[string]bool) bool {
	return ast.IsTypeAliasDeclaration(statement) && statement.Name() != nil && structModelNames[statement.Name().Text()]
}

func hasEffectSchemaOpaqueHeritage(classDeclaration *ast.Node) bool {
	heritageType := getFirstHeritageType(classDeclaration)
	if heritageType == nil {
		return false
	}
	expression := heritageType.AsExpressionWithTypeArguments().Expression
	if expression == nil {
		return false
	}
	if ast.IsCallExpression(expression) {
		expression = expression.AsCallExpression().Expression
	}
	if ast.IsCallExpression(expression) {
		expression = expression.AsCallExpression().Expression
	}
	return isEffectSchemaOpaqueExpression(expression)
}

func getFirstHeritageType(classDeclaration *ast.Node) *ast.Node {
	if classDeclaration == nil || !ast.IsClassDeclaration(classDeclaration) || classDeclaration.AsClassDeclaration().HeritageClauses == nil {
		return nil
	}
	clauses := classDeclaration.AsClassDeclaration().HeritageClauses.Nodes
	if len(clauses) == 0 || clauses[0].AsHeritageClause().Types == nil || len(clauses[0].AsHeritageClause().Types.Nodes) == 0 {
		return nil
	}
	return clauses[0].AsHeritageClause().Types.Nodes[0]
}

func isEffectSchemaOpaqueExpression(expression *ast.Node) bool {
	return getEffectSchemaCtorFacadeName(expression) != ""
}

// Maps a schema-model heritage constructor (S.Opaque(...), S.Class(...), S.ErrorClass(...), ...)
// to the effect-app facade type its base should be rewritten to. Returns "" for non-model
// constructors. Opaque family (incl. requests) -> OpaqueFacade; class family -> OpaqueClassFacade;
// error family -> OpaqueErrorFacadeClass.
func getEffectSchemaCtorFacadeName(expression *ast.Node) string {
	if !ast.IsPropertyAccessExpression(expression) || expression.Name() == nil || expression.Expression() == nil {
		return ""
	}
	left := expression.Expression()
	if !ast.IsIdentifier(left) || (left.Text() != "S" && left.Text() != "Schema") {
		return ""
	}
	switch expression.Name().Text() {
	case "Opaque", "OpaqueFacade":
		return "OpaqueFacade"
	case "Class", "TaggedClass":
		return "OpaqueClassFacade"
	case "ErrorClass", "TaggedErrorClass":
		return "OpaqueErrorFacadeClass"
	default:
		return ""
	}
}

func getEffectSchemaClassFacadeName(classDeclaration *ast.Node) string {
	heritageType := getFirstHeritageType(classDeclaration)
	if heritageType == nil {
		return "OpaqueFacade"
	}
	expression := heritageType.AsExpressionWithTypeArguments().Expression
	for expression != nil && ast.IsCallExpression(expression) {
		expression = expression.AsCallExpression().Expression
	}
	if expression != nil {
		if name := getEffectSchemaCtorFacadeName(expression); name != "" {
			return name
		}
	}
	return "OpaqueFacade"
}

func isEffectSchemaModelNamespace(statement *ast.Node) bool {
	if !ast.IsModuleDeclaration(statement) || statement.Name() == nil || !ast.IsIdentifier(statement.Name()) || statement.AsModuleDeclaration().Body == nil || statement.AsModuleDeclaration().Body.Kind != ast.KindModuleBlock {
		return false
	}
	return core.Some(statement.AsModuleDeclaration().Body.AsModuleBlock().Statements.Nodes, isEffectSchemaStructNestedEncodedInterface)
}

func isEffectSchemaMaterializedModelNamespace(statement *ast.Node, classes map[string]*ast.Node) bool {
	name := moduleDeclarationIdentifierName(statement)
	if name == "" || classes[name] == nil || statement.AsModuleDeclaration().Body == nil || statement.AsModuleDeclaration().Body.Kind != ast.KindModuleBlock {
		return false
	}
	return core.Some(statement.AsModuleDeclaration().Body.AsModuleBlock().Statements.Nodes, isEffectSchemaEncodedInterface)
}

func moduleDeclarationIdentifierName(statement *ast.Node) string {
	if !ast.IsModuleDeclaration(statement) || statement.Name() == nil || !ast.IsIdentifier(statement.Name()) {
		return ""
	}
	return statement.Name().Text()
}

func isEffectSchemaStructNestedEncodedInterface(statement *ast.Node) bool {
	if !ast.IsInterfaceDeclaration(statement) || statement.Name() == nil || statement.Name().Text() != "Encoded" || statement.AsInterfaceDeclaration().HeritageClauses == nil {
		return false
	}
	clauses := statement.AsInterfaceDeclaration().HeritageClauses.Nodes
	if len(clauses) != 1 || clauses[0].AsHeritageClause().Types == nil || len(clauses[0].AsHeritageClause().Types.Nodes) != 1 {
		return false
	}
	heritageType := clauses[0].AsHeritageClause().Types.Nodes[0]
	expression := heritageType.AsExpressionWithTypeArguments().Expression
	typeArguments := heritageType.AsExpressionWithTypeArguments().TypeArguments
	return ast.IsPropertyAccessExpression(expression) &&
		expression.Name() != nil &&
		expression.Name().Text() == "StructNestedEncoded" &&
		typeArguments != nil &&
		len(typeArguments.Nodes) == 1 &&
		typeArguments.Nodes[0].Kind == ast.KindTypeQuery
}

func isEffectSchemaEncodedInterface(statement *ast.Node) bool {
	return ast.IsInterfaceDeclaration(statement) && statement.Name() != nil && statement.Name().Text() == "Encoded"
}

func (tx *DeclarationTransformer) canCreateEffectSchemaGeneratedNamespace(classDeclaration *ast.Node) bool {
	return tx.createEffectSchemaEncodedDeclaration(classDeclaration) != nil
}

func getEffectSchemaClassName(statement *ast.Node) string {
	if ast.IsClassDeclaration(statement) && statement.Name() != nil {
		return statement.Name().Text()
	}
	return ""
}

func hasTopLevelInterface(statements *ast.StatementList, name string) bool {
	return core.Some(statements.Nodes, func(statement *ast.Node) bool {
		return ast.IsInterfaceDeclaration(statement) && statement.Name() != nil && statement.Name().Text() == name
	})
}

func hasTopLevelNamespace(statements *ast.StatementList, name string) bool {
	return core.Some(statements.Nodes, func(statement *ast.Node) bool {
		return moduleDeclarationIdentifierName(statement) == name
	})
}

func (tx *DeclarationTransformer) createEffectSchemaTypeInterface(classDeclaration *ast.Node) *ast.Node {
	if classDeclaration == nil || classDeclaration.Name() == nil {
		return nil
	}
	literal := tx.resolver.CreateTypeLiteralOfClassDeclaration(tx.EmitContext(), classDeclaration, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	if literal == nil || !ast.IsTypeLiteralNode(literal) {
		return nil
	}
	return tx.Factory().NewInterfaceDeclaration(
		tx.createEffectSchemaNamespaceModifiers(classDeclaration, false),
		tx.Factory().NewIdentifier(classDeclaration.Name().Text()),
		nil,
		nil,
		tx.Factory().NewNodeList(literal.AsTypeLiteralNode().Members.Nodes),
	)
}

func (tx *DeclarationTransformer) updateEffectSchemaNamespaceDeclaration(namespace *ast.Node, classDeclaration *ast.Node) *ast.Node {
	body := namespace.AsModuleDeclaration().Body
	if body == nil || body.Kind != ast.KindModuleBlock {
		return nil
	}
	existing := map[string]bool{}
	kept := make([]*ast.Node, 0, len(body.AsModuleBlock().Statements.Nodes))
	for _, statement := range body.AsModuleBlock().Statements.Nodes {
		if (ast.IsInterfaceDeclaration(statement) || ast.IsTypeAliasDeclaration(statement)) && statement.Name() != nil && statement.Name().Text() != "Encoded" {
			name := statement.Name().Text()
			existing[name] = true
			if name == "Make" || name == "DecodingServices" || name == "EncodingServices" {
				continue
			}
		}
		kept = append(kept, statement)
	}

	additions := []*ast.Node{}
	if makeDeclaration := tx.createEffectSchemaMakeDeclaration(classDeclaration); makeDeclaration != nil {
		additions = append(additions, makeDeclaration)
	}
	if decodingServices := tx.createEffectSchemaServiceDeclaration(classDeclaration, "DecodingServices"); decodingServices != nil {
		additions = append(additions, decodingServices)
	}
	if encodingServices := tx.createEffectSchemaServiceDeclaration(classDeclaration, "EncodingServices"); encodingServices != nil {
		additions = append(additions, encodingServices)
	}
	if len(additions) == 0 && len(existing) == 0 {
		return nil
	}

	statements := tx.Factory().NewNodeList(append(kept, additions...))
	moduleBlock := tx.Factory().UpdateModuleBlock(body.AsModuleBlock(), statements)
	return tx.Factory().UpdateModuleDeclaration(namespace.AsModuleDeclaration(), namespace.Modifiers(), namespace.AsModuleDeclaration().Keyword, namespace.Name(), moduleBlock)
}

func (tx *DeclarationTransformer) createEffectSchemaGeneratedNamespaceDeclaration(modelName string, classDeclaration *ast.Node) *ast.Node {
	encoded := tx.createEffectSchemaEncodedDeclaration(classDeclaration)
	if encoded == nil {
		return nil
	}
	statements := []*ast.Node{encoded}
	if makeDeclaration := tx.createEffectSchemaMakeDeclaration(classDeclaration); makeDeclaration != nil {
		statements = append(statements, makeDeclaration)
	}
	if decodingServices := tx.createEffectSchemaServiceDeclaration(classDeclaration, "DecodingServices"); decodingServices != nil {
		statements = append(statements, decodingServices)
	}
	if encodingServices := tx.createEffectSchemaServiceDeclaration(classDeclaration, "EncodingServices"); encodingServices != nil {
		statements = append(statements, encodingServices)
	}
	return tx.Factory().NewModuleDeclaration(
		tx.createEffectSchemaNamespaceModifiers(classDeclaration, true),
		ast.KindNamespaceKeyword,
		tx.Factory().NewIdentifier(modelName),
		tx.Factory().NewModuleBlock(tx.Factory().NewNodeList(statements)),
	)
}

func (tx *DeclarationTransformer) createEffectSchemaNamespaceModifiers(classDeclaration *ast.Node, includeDeclare bool) *ast.ModifierList {
	modifiers := []*ast.Node{}
	if ast.HasSyntacticModifier(classDeclaration, ast.ModifierFlagsExport) {
		modifiers = append(modifiers, tx.Factory().NewModifier(ast.KindExportKeyword))
	}
	if includeDeclare {
		modifiers = append(modifiers, tx.Factory().NewModifier(ast.KindDeclareKeyword))
	}
	if len(modifiers) == 0 {
		return nil
	}
	return tx.Factory().NewModifierList(modifiers)
}

func (tx *DeclarationTransformer) createEffectSchemaEncodedDeclaration(classDeclaration *ast.Node) *ast.Node {
	encodedType := tx.resolver.CreateTypeOfClassStaticProperty(tx.EmitContext(), classDeclaration, "Encoded", tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	if encodedType == nil {
		return nil
	}
	if ast.IsTypeLiteralNode(encodedType) {
		return tx.Factory().NewInterfaceDeclaration(nil, tx.Factory().NewIdentifier("Encoded"), nil, nil, tx.Factory().NewNodeList(encodedType.AsTypeLiteralNode().Members.Nodes))
	}
	return tx.Factory().NewTypeAliasDeclaration(nil, tx.Factory().NewIdentifier("Encoded"), nil, encodedType)
}

func (tx *DeclarationTransformer) createEffectSchemaMakeDeclaration(classDeclaration *ast.Node) *ast.Node {
	makeType := tx.resolver.CreateMakeTypeOfClassDeclaration(tx.EmitContext(), classDeclaration, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	if makeType == nil {
		makeType = tx.resolver.CreateTypeOfClassStaticProperty(tx.EmitContext(), classDeclaration, "~type.make.in", tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	}
	if makeType == nil {
		return nil
	}
	if ast.IsTypeLiteralNode(makeType) {
		return tx.Factory().NewInterfaceDeclaration(nil, tx.Factory().NewIdentifier("Make"), nil, nil, tx.Factory().NewNodeList(makeType.AsTypeLiteralNode().Members.Nodes))
	}
	return tx.Factory().NewTypeAliasDeclaration(nil, tx.Factory().NewIdentifier("Make"), nil, makeType)
}

func (tx *DeclarationTransformer) createEffectSchemaServiceDeclaration(classDeclaration *ast.Node, name string) *ast.Node {
	resolved := tx.resolver.CreateTypeOfClassStaticProperty(tx.EmitContext(), classDeclaration, name, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	if resolved == nil {
		return nil
	}
	serviceType := resolved
	if resolved.Kind == ast.KindAnyKeyword {
		serviceType = tx.Factory().NewKeywordTypeNode(ast.KindNeverKeyword)
	}
	return tx.Factory().NewTypeAliasDeclaration(nil, tx.Factory().NewIdentifier(name), nil, serviceType)
}

func getEffectSchemaBaseModelName(statement *ast.Node) string {
	if !ast.IsVariableStatement(statement) || statement.AsVariableStatement().DeclarationList == nil {
		return ""
	}
	declarations := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes
	if len(declarations) != 1 || declarations[0].Name() == nil || !ast.IsIdentifier(declarations[0].Name()) {
		return ""
	}
	name := declarations[0].Name().Text()
	if !strings.HasSuffix(name, "_base") {
		return ""
	}
	baseName := strings.TrimSuffix(name, "_base")
	return strings.TrimPrefix(baseName, "__")
}

func (tx *DeclarationTransformer) updateEffectSchemaBaseDeclaration(statement *ast.Node, modelName string, classDeclaration *ast.Node, usesIntermediateClass bool) *ast.Node {
	declaration := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes[0]
	name := declaration.Name()
	if usesIntermediateClass {
		name = tx.Factory().NewIdentifier("__" + modelName + "_base")
	}
	updatedDeclaration := tx.Factory().UpdateVariableDeclaration(
		declaration.AsVariableDeclaration(),
		name,
		declaration.AsVariableDeclaration().ExclamationToken,
		tx.createEffectSchemaFacadeBaseType(modelName, classDeclaration, declaration.AsVariableDeclaration().Type),
		declaration.AsVariableDeclaration().Initializer,
	)
	declarations := tx.Factory().NewNodeList([]*ast.Node{updatedDeclaration})
	declarationList := tx.Factory().UpdateVariableDeclarationList(statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList(), declarations, statement.AsVariableStatement().DeclarationList.Flags)
	return tx.Factory().UpdateVariableStatement(statement.AsVariableStatement(), statement.Modifiers(), declarationList)
}

func getEffectSchemaRequestBaseInfo(statement *ast.Node) (effectSchemaRequestBaseInfo, bool) {
	if !ast.IsVariableStatement(statement) || statement.AsVariableStatement().DeclarationList == nil {
		return effectSchemaRequestBaseInfo{}, false
	}
	declarations := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes
	if len(declarations) != 1 || declarations[0].Name() == nil || !ast.IsIdentifier(declarations[0].Name()) || declarations[0].Type() == nil {
		return effectSchemaRequestBaseInfo{}, false
	}
	modelName := getEffectSchemaBaseModelName(statement)
	if modelName == "" {
		return effectSchemaRequestBaseInfo{}, false
	}
	opaqueType := getEffectSchemaRequestOpaqueType(declarations[0].Type(), modelName)
	if opaqueType == nil || opaqueType.AsTypeReferenceNode().TypeArguments == nil || len(opaqueType.AsTypeReferenceNode().TypeArguments.Nodes) < 4 {
		return effectSchemaRequestBaseInfo{}, false
	}
	return effectSchemaRequestBaseInfo{modelName: modelName, brand: opaqueType.AsTypeReferenceNode().TypeArguments.Nodes[3]}, true
}

func getEffectSchemaRequestOpaqueType(typeNode *ast.Node, modelName string) *ast.Node {
	if isEffectSchemaRequestOpaqueType(typeNode, modelName) {
		return typeNode
	}
	if typeNode == nil || typeNode.Kind != ast.KindIntersectionType {
		return nil
	}
	for _, part := range typeNode.AsIntersectionTypeNode().Types.Nodes {
		if isEffectSchemaRequestOpaqueType(part, modelName) {
			return part
		}
	}
	return nil
}

func isEffectSchemaRequestOpaqueType(typeNode *ast.Node, modelName string) bool {
	if typeNode == nil || typeNode.Kind != ast.KindTypeReference {
		return false
	}
	typeName := typeNode.AsTypeReferenceNode().TypeName
	if typeName == nil || typeName.Kind != ast.KindQualifiedName {
		return false
	}
	qualifiedName := typeName.AsQualifiedName()
	if qualifiedName.Right.Text() != "Opaque" || qualifiedName.Left == nil || qualifiedName.Left.Kind != ast.KindIdentifier || qualifiedName.Left.Text() != "S" {
		return false
	}
	typeArguments := typeNode.AsTypeReferenceNode().TypeArguments
	if typeArguments == nil || len(typeArguments.Nodes) < 4 {
		return false
	}
	return isEffectSchemaNamedTypeReference(typeArguments.Nodes[0], modelName) && isEffectSchemaExtendedSchemaNoEncodedType(typeArguments.Nodes[1])
}

func isEffectSchemaNamedTypeReference(typeNode *ast.Node, name string) bool {
	return typeNode != nil && typeNode.Kind == ast.KindTypeReference && typeNode.AsTypeReferenceNode().TypeName.Kind == ast.KindIdentifier && typeNode.AsTypeReferenceNode().TypeName.Text() == name
}

func isEffectSchemaExtendedSchemaNoEncodedType(typeNode *ast.Node) bool {
	if typeNode == nil || typeNode.Kind != ast.KindTypeQuery {
		return false
	}
	exprName := typeNode.AsTypeQueryNode().ExprName
	if exprName == nil || exprName.Kind != ast.KindQualifiedName {
		return false
	}
	qualifiedName := exprName.AsQualifiedName()
	return qualifiedName.Right.Text() == "ExtendedSchemaNoEncoded" && qualifiedName.Left != nil && qualifiedName.Left.Kind == ast.KindIdentifier && qualifiedName.Left.Text() == "S"
}

func (tx *DeclarationTransformer) updateEffectSchemaRequestBaseDeclaration(statement *ast.Node, info effectSchemaRequestBaseInfo) *ast.Node {
	declaration := statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList().Declarations.Nodes[0]
	updatedType := tx.updateEffectSchemaRequestBaseType(declaration.Type(), info)
	if updatedType == nil {
		return nil
	}
	updatedDeclaration := tx.Factory().UpdateVariableDeclaration(
		declaration.AsVariableDeclaration(),
		declaration.Name(),
		declaration.AsVariableDeclaration().ExclamationToken,
		updatedType,
		declaration.AsVariableDeclaration().Initializer,
	)
	declarations := tx.Factory().NewNodeList([]*ast.Node{updatedDeclaration})
	declarationList := tx.Factory().UpdateVariableDeclarationList(statement.AsVariableStatement().DeclarationList.AsVariableDeclarationList(), declarations, statement.AsVariableStatement().DeclarationList.Flags)
	return tx.Factory().UpdateVariableStatement(statement.AsVariableStatement(), statement.Modifiers(), declarationList)
}

func (tx *DeclarationTransformer) updateEffectSchemaRequestBaseType(typeNode *ast.Node, info effectSchemaRequestBaseInfo) *ast.Node {
	if isEffectSchemaRequestOpaqueType(typeNode, info.modelName) {
		return tx.createEffectSchemaFacadeTypeReference(info.modelName, info.brand, "OpaqueFacade")
	}
	if typeNode == nil || typeNode.Kind != ast.KindIntersectionType {
		return nil
	}
	changed := false
	types := make([]*ast.Node, 0, len(typeNode.AsIntersectionTypeNode().Types.Nodes))
	for _, part := range typeNode.AsIntersectionTypeNode().Types.Nodes {
		if isEffectSchemaRequestOpaqueType(part, info.modelName) {
			changed = true
			types = append(types, tx.createEffectSchemaFacadeTypeReference(info.modelName, info.brand, "OpaqueFacade"))
		} else {
			types = append(types, part)
		}
	}
	if !changed {
		return nil
	}
	return tx.Factory().UpdateIntersectionTypeNode(typeNode.AsIntersectionTypeNode(), tx.Factory().NewNodeList(types))
}

func needsEffectSchemaIntermediateClass(classDeclaration *ast.Node) bool {
	return classDeclaration != nil && classDeclaration.ClassLikeData() != nil && len(classDeclaration.ClassLikeData().Members.Nodes) > 0
}

func (tx *DeclarationTransformer) createEffectSchemaIntermediateClass(modelName string) *ast.Node {
	return tx.Factory().NewClassDeclaration(
		tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindDeclareKeyword)}),
		tx.Factory().NewIdentifier("__"+modelName),
		nil,
		tx.Factory().NewNodeList([]*ast.Node{
			tx.Factory().NewHeritageClause(ast.KindExtendsKeyword, tx.Factory().NewNodeList([]*ast.Node{
				tx.Factory().NewExpressionWithTypeArguments(tx.Factory().NewIdentifier("__"+modelName+"_base"), nil),
			})),
		}),
		tx.Factory().NewNodeList([]*ast.Node{}),
	)
}

func (tx *DeclarationTransformer) updateEffectSchemaClassDeclaration(classDeclaration *ast.Node, modelName string) *ast.Node {
	return tx.Factory().UpdateClassDeclaration(
		classDeclaration.AsClassDeclaration(),
		classDeclaration.Modifiers(),
		classDeclaration.Name(),
		classDeclaration.AsClassDeclaration().TypeParameters,
		tx.Factory().NewNodeList([]*ast.Node{
			tx.Factory().NewHeritageClause(ast.KindExtendsKeyword, tx.Factory().NewNodeList([]*ast.Node{
				tx.Factory().NewExpressionWithTypeArguments(tx.Factory().NewIdentifier("__"+modelName), nil),
			})),
		}),
		classDeclaration.AsClassDeclaration().Members,
	)
}

func (tx *DeclarationTransformer) createEffectSchemaFacadeBaseType(modelName string, classDeclaration *ast.Node, baseType *ast.Node) *ast.Node {
	facadeName := getEffectSchemaClassFacadeName(classDeclaration)
	brandType := tx.getEffectSchemaFacadeBrandType(baseType, facadeName)
	return tx.Factory().NewIntersectionTypeNode(tx.Factory().NewNodeList([]*ast.Node{
		tx.createEffectSchemaFacadeTypeReference(modelName, brandType, facadeName),
		tx.Factory().NewTypeLiteralNode(tx.Factory().NewNodeList(tx.createEffectSchemaStaticMembers(classDeclaration))),
	}))
}

// The facade's Brand (last type arg). For the class/error families the source base is
// S.EnhancedClass<Self, Schema, Inherited> — the 3rd arg is the brand (e.g. Cause.YieldableError
// for errors); preserve it. The Opaque family carries no brand on the base, so use {}.
func (tx *DeclarationTransformer) getEffectSchemaFacadeBrandType(baseType *ast.Node, facadeName string) *ast.Node {
	empty := tx.Factory().NewTypeLiteralNode(tx.Factory().NewNodeList([]*ast.Node{}))
	if facadeName == "OpaqueFacade" || baseType == nil {
		return empty
	}
	typeNode := baseType
	if ast.IsIntersectionTypeNode(typeNode) {
		nodes := typeNode.AsIntersectionTypeNode().Types.Nodes
		if len(nodes) == 0 {
			return empty
		}
		typeNode = nodes[0]
	}
	if typeNode.Kind != ast.KindTypeReference || typeNode.AsTypeReferenceNode().TypeArguments == nil {
		return empty
	}
	args := typeNode.AsTypeReferenceNode().TypeArguments.Nodes
	if len(args) >= 3 {
		return args[2]
	}
	return empty
}

func (tx *DeclarationTransformer) createEffectSchemaFacadeTypeReference(modelName string, brandType *ast.Node, facadeName string) *ast.Node {
	model := tx.Factory().NewIdentifier(modelName)
	return tx.Factory().NewTypeReferenceNode(
		tx.Factory().NewQualifiedName(tx.Factory().NewIdentifier("S"), tx.Factory().NewIdentifier(facadeName)),
		tx.Factory().NewNodeList([]*ast.Node{
			tx.Factory().NewTypeReferenceNode(model, nil),
			tx.Factory().NewTypeReferenceNode(tx.Factory().NewQualifiedName(model, tx.Factory().NewIdentifier("Encoded")), nil),
			tx.Factory().NewTypeReferenceNode(tx.Factory().NewQualifiedName(model, tx.Factory().NewIdentifier("Make")), nil),
			tx.Factory().NewTypeReferenceNode(tx.Factory().NewQualifiedName(model, tx.Factory().NewIdentifier("DecodingServices")), nil),
			tx.Factory().NewTypeReferenceNode(tx.Factory().NewQualifiedName(model, tx.Factory().NewIdentifier("EncodingServices")), nil),
			brandType,
		}),
	)
}

func (tx *DeclarationTransformer) createEffectSchemaStaticMembers(classDeclaration *ast.Node) []*ast.Node {
	members := []*ast.Node{}
	// NOTE: `identifier` (generic `string`) is intentionally NOT emitted here — it lives on the
	// facade interfaces (OpaqueFacade/OpaqueClassFacade/OpaqueErrorFacadeClass) in effect-app.
	// Only per-model, precisely-typed statics belong here.
	tx.addSchemaStaticMember(&members, classDeclaration, "fields", true)
	tx.addSchemaStaticMember(&members, classDeclaration, "mapFields", false)
	tx.addSchemaStaticMember(&members, classDeclaration, "to", true)
	tx.addSchemaStaticMember(&members, classDeclaration, "from", true)
	tx.addSchemaStaticMember(&members, classDeclaration, "copy", true)
	return members
}

func (tx *DeclarationTransformer) addSchemaStaticMember(members *[]*ast.Node, classDeclaration *ast.Node, name string, readonly bool) {
	typeNode := tx.resolver.CreateTypeOfClassStaticProperty(tx.EmitContext(), classDeclaration, name, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker)
	if typeNode == nil || typeNode.Kind == ast.KindAnyKeyword {
		return
	}
	var modifiers *ast.ModifierList
	if readonly {
		modifiers = tx.Factory().NewModifierList([]*ast.Node{tx.Factory().NewModifier(ast.KindReadonlyKeyword)})
	}
	*members = append(*members, tx.Factory().NewPropertySignatureDeclaration(modifiers, tx.Factory().NewIdentifier(name), nil, typeNode, nil))
}

func (tx *DeclarationTransformer) ensureModifiers(node *ast.Node) *ast.ModifierList {
	currentFlags := ast.GetCombinedModifierFlags(tx.EmitContext().ParseNode(node)) & ast.ModifierFlagsAll
	newFlags := tx.ensureModifierFlags(node)
	if currentFlags == newFlags {
		// Elide decorators
		mods := node.Modifiers()
		if mods == nil {
			return mods
		}
		if canReuseModifierNodes(mods.Nodes) {
			return tx.Factory().NewModifierList(core.Filter(mods.Nodes, ast.IsModifier))
		}
	}
	result := ast.CreateModifiersFromModifierFlags(newFlags, tx.Factory().NewModifier)
	if len(result) == 0 {
		return nil
	}
	return tx.Factory().NewModifierList(result)
}

func (tx *DeclarationTransformer) ensureModifierFlags(node *ast.Node) ast.ModifierFlags {
	mask := ast.ModifierFlagsAll ^ (ast.ModifierFlagsPublic | ast.ModifierFlagsAsync | ast.ModifierFlagsOverride) // No async and override modifiers in declaration files
	additions := ast.ModifierFlagsNone
	if tx.needsDeclare && !isAlwaysType(node) {
		additions = ast.ModifierFlagsAmbient
	}
	parentIsFile := node.Parent.Kind == ast.KindSourceFile
	if !parentIsFile {
		mask ^= ast.ModifierFlagsAmbient
		additions = ast.ModifierFlagsNone
	}
	if ast.IsImplicitlyExportedJSDocDeclaration(node) {
		additions |= ast.ModifierFlagsExport
	}
	return maskModifierFlags(node, mask, additions)
}

func (tx *DeclarationTransformer) ensureTypeParams(node *ast.Node, params *ast.TypeParameterList) *ast.TypeParameterList {
	if tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(node), ast.ModifierFlagsPrivate) != 0 {
		return nil
	}
	var typeParameters *ast.TypeParameterList
	if typeParameters = tx.Visitor().VisitNodes(params); typeParameters != nil {
		return typeParameters
	}
	oldErrorNameNode := tx.state.errorNameNode
	tx.state.errorNameNode = node.Name()
	var oldDiag GetSymbolAccessibilityDiagnostic
	if !tx.suppressNewDiagnosticContexts {
		oldDiag = tx.state.getSymbolAccessibilityDiagnostic
		if canProduceDiagnostics(node) {
			tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(node)
		}
	}

	if data := node.FunctionLikeData(); data != nil && data.FullSignature != nil {
		if nodes := tx.resolver.CreateTypeParametersOfSignatureDeclaration(tx.EmitContext(), node, tx.enclosingDeclaration, declarationEmitNodeBuilderFlags, declarationEmitInternalNodeBuilderFlags, tx.tracker); nodes != nil {
			typeParameters = &ast.TypeParameterList{
				Loc:   node.Loc,
				Nodes: nodes,
			}
		}
	}

	tx.state.errorNameNode = oldErrorNameNode
	if !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	}
	return typeParameters
}

func (tx *DeclarationTransformer) updateParamList(node *ast.Node, params *ast.ParameterList) *ast.ParameterList {
	if tx.host.GetEffectiveDeclarationFlags(tx.EmitContext().ParseNode(node), ast.ModifierFlagsPrivate) != 0 || len(params.Nodes) == 0 {
		return tx.Factory().NewNodeList([]*ast.Node{})
	}
	results := make([]*ast.Node, len(params.Nodes))
	for i, p := range params.Nodes {
		results[i] = tx.ensureParameter(p.AsParameterDeclaration())
	}
	return tx.Factory().NewNodeList(results)
}

func (tx *DeclarationTransformer) ensureParameter(p *ast.ParameterDeclaration) *ast.Node {
	oldDiag := tx.state.getSymbolAccessibilityDiagnostic
	if !tx.suppressNewDiagnosticContexts {
		tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(p.AsNode())
	}
	var questionToken *ast.TokenNode
	if tx.resolver.IsOptionalParameter(p.AsNode()) {
		if p.QuestionToken != nil {
			questionToken = p.QuestionToken
		} else {
			questionToken = tx.Factory().NewToken(ast.KindQuestionToken)
		}
	}
	result := tx.Factory().UpdateParameterDeclaration(
		p,
		nil,
		p.DotDotDotToken,
		tx.bindingNameVisitor.VisitNode(p.Name()),
		questionToken,
		tx.ensureType(p.AsNode(), true),
		tx.ensureNoInitializer(p.AsNode()),
	)
	tx.state.getSymbolAccessibilityDiagnostic = oldDiag
	return result
}

func (tx *DeclarationTransformer) ensureNoInitializer(node *ast.Node) *ast.Node {
	if tx.shouldPrintWithInitializer(node) {
		unwrappedInitializer := unwrapParenthesizedExpression(node.Initializer())
		if !ast.IsPrimitiveLiteralValue(unwrappedInitializer, true) {
			tx.tracker.ReportInferenceFallback(node)
		}
		return tx.resolver.CreateLiteralConstValue(tx.EmitContext(), tx.EmitContext().ParseNode(node), tx.tracker)
	}
	return nil
}

func (tx *DeclarationTransformer) visitBindingName(node *ast.Node) *ast.Node {
	switch node.Kind {
	case ast.KindIdentifier, ast.KindOmittedExpression:
		return node
	case ast.KindArrayBindingPattern, ast.KindObjectBindingPattern:
		return node.VisitEachChild(tx.bindingNameVisitor)
	case ast.KindBindingElement:
		if node.PropertyName() != nil && ast.IsComputedPropertyName(node.PropertyName()) && ast.IsEntityNameExpression(node.PropertyName().Expression()) {
			tx.checkEntityNameVisibility(node.PropertyName().Expression(), tx.enclosingDeclaration)
		}
		return tx.Factory().UpdateBindingElement(node.AsBindingElement(), node.AsBindingElement().DotDotDotToken, node.PropertyName(), tx.bindingNameVisitor.VisitNode(node.Name()), nil /*initializer*/)
	default:
		return node
	}
}

func (tx *DeclarationTransformer) transformImportEqualsDeclaration(decl *ast.ImportEqualsDeclaration) *ast.Node {
	if !tx.resolver.IsDeclarationVisible(decl.AsNode()) {
		return nil
	}
	if decl.ModuleReference.Kind == ast.KindExternalModuleReference {
		// Rewrite external module names if necessary
		specifier := ast.GetExternalModuleImportEqualsDeclarationExpression(decl.AsNode())
		return tx.Factory().UpdateImportEqualsDeclaration(
			decl,
			decl.Modifiers(),
			decl.IsTypeOnly,
			decl.Name(),
			tx.Factory().UpdateExternalModuleReference(decl.ModuleReference.AsExternalModuleReference(), tx.rewriteModuleSpecifier(decl.AsNode(), specifier)),
		)
	} else {
		oldDiag := tx.state.getSymbolAccessibilityDiagnostic
		tx.state.getSymbolAccessibilityDiagnostic = createGetSymbolAccessibilityDiagnosticForNode(decl.AsNode())
		tx.checkEntityNameVisibility(decl.ModuleReference, tx.enclosingDeclaration)
		tx.state.getSymbolAccessibilityDiagnostic = oldDiag
		return decl.AsNode()
	}
}

func (tx *DeclarationTransformer) transformImportDeclaration(decl *ast.ImportDeclaration) *ast.Node {
	if decl.ImportClause == nil {
		// import "mod" - possibly needed for side effects? (global interface patches, module augmentations, etc)
		return tx.Factory().UpdateImportDeclaration(
			decl,
			decl.Modifiers(),
			decl.ImportClause,
			tx.rewriteModuleSpecifier(decl.AsNode(), decl.ModuleSpecifier),
			tx.tryGetResolutionModeOverride(decl.Attributes),
		)
	}
	phaseModifier := decl.ImportClause.AsImportClause().PhaseModifier
	if phaseModifier == ast.KindDeferKeyword {
		phaseModifier = ast.KindUnknown
	}
	// The `importClause` visibility corresponds to the default's visibility.
	var visibleDefaultBinding *ast.Node
	if decl.ImportClause != nil && decl.ImportClause.Name() != nil && tx.resolver.IsDeclarationVisible(decl.ImportClause) {
		visibleDefaultBinding = decl.ImportClause.Name()
	}
	if decl.ImportClause.AsImportClause().NamedBindings == nil {
		// No named bindings (either namespace or list), meaning the import is just default or should be elided
		if visibleDefaultBinding == nil {
			return nil
		}
		return tx.Factory().UpdateImportDeclaration(
			decl,
			decl.Modifiers(),
			tx.Factory().UpdateImportClause(
				decl.ImportClause.AsImportClause(),
				phaseModifier,
				visibleDefaultBinding,
				/*namedBindings*/ nil,
			),
			tx.rewriteModuleSpecifier(decl.AsNode(), decl.ModuleSpecifier),
			tx.tryGetResolutionModeOverride(decl.Attributes),
		)
	}
	if decl.ImportClause.AsImportClause().NamedBindings.Kind == ast.KindNamespaceImport {
		// Namespace import (optionally with visible default)
		var namedBindings *ast.Node
		if tx.resolver.IsDeclarationVisible(decl.ImportClause.AsImportClause().NamedBindings) {
			namedBindings = decl.ImportClause.AsImportClause().NamedBindings
		}
		if visibleDefaultBinding == nil && namedBindings == nil {
			return nil
		}
		return tx.Factory().UpdateImportDeclaration(
			decl,
			decl.Modifiers(),
			tx.Factory().UpdateImportClause(
				decl.ImportClause.AsImportClause(),
				phaseModifier,
				visibleDefaultBinding,
				namedBindings,
			),
			tx.rewriteModuleSpecifier(decl.AsNode(), decl.ModuleSpecifier),
			tx.tryGetResolutionModeOverride(decl.Attributes),
		)
	}
	// Named imports (optionally with visible default)
	bindingList := core.Filter(
		decl.ImportClause.AsImportClause().NamedBindings.Elements(),
		func(b *ast.Node) bool {
			return tx.resolver.IsDeclarationVisible(b)
		},
	)
	if len(bindingList) > 0 || visibleDefaultBinding != nil {
		var namedImports *ast.Node
		if len(bindingList) > 0 {
			namedImports = tx.Factory().UpdateNamedImports(
				decl.ImportClause.AsImportClause().NamedBindings.AsNamedImports(),
				tx.Factory().NewNodeList(bindingList),
			)
		}
		return tx.Factory().UpdateImportDeclaration(
			decl,
			decl.Modifiers(),
			tx.Factory().UpdateImportClause(
				decl.ImportClause.AsImportClause(),
				phaseModifier,
				visibleDefaultBinding,
				namedImports,
			),
			tx.rewriteModuleSpecifier(decl.AsNode(), decl.ModuleSpecifier),
			tx.tryGetResolutionModeOverride(decl.Attributes),
		)
	}
	// Augmentation of export depends on import
	if tx.resolver.IsImportRequiredByAugmentation(decl) {
		if tx.state.isolatedDeclarations {
			tx.state.addDiagnostic(createDiagnosticForNode(decl.AsNode(), diagnostics.Declaration_emit_for_this_file_requires_preserving_this_import_for_augmentations_This_is_not_supported_with_isolatedDeclarations))
		}
		return tx.Factory().UpdateImportDeclaration(
			decl,
			decl.Modifiers(),
			/*importClause*/ nil,
			tx.rewriteModuleSpecifier(decl.AsNode(), decl.ModuleSpecifier),
			tx.tryGetResolutionModeOverride(decl.Attributes),
		)
	}
	// Nothing visible
	return nil
}

func (tx *DeclarationTransformer) transformJSDocTypeExpression(input *ast.JSDocTypeExpression) *ast.Node {
	return tx.Visitor().Visit(input.Type)
}

func (tx *DeclarationTransformer) transformJSDocTypeLiteral(input *ast.JSDocTypeLiteral) *ast.Node {
	members, _ := tx.Visitor().VisitSlice(input.JSDocPropertyTags)
	replacement := tx.Factory().NewTypeLiteralNode(tx.Factory().NewNodeList(members))
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) transformJSDocPropertyTag(input *ast.JSDocParameterOrPropertyTag) *ast.Node {
	replacement := tx.Factory().NewPropertySignatureDeclaration(
		nil,
		tx.Visitor().Visit(input.TagName),
		nil,
		tx.Visitor().Visit(input.TypeExpression),
		nil,
	)
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) transformJSDocAllType(input *ast.JSDocAllType) *ast.Node {
	replacement := tx.Factory().NewKeywordTypeNode(ast.KindAnyKeyword)
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) transformJSDocNullableType(input *ast.JSDocNullableType) *ast.Node {
	replacement := tx.Factory().NewUnionTypeNode(tx.Factory().NewNodeList([]*ast.Node{
		tx.Visitor().Visit(input.Type),
		tx.Factory().NewLiteralTypeNode(tx.Factory().NewKeywordExpression(ast.KindNullKeyword)),
	}))
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) transformJSDocNonNullableType(input *ast.JSDocNonNullableType) *ast.Node {
	return tx.Visitor().Visit(input.Type)
}

func (tx *DeclarationTransformer) transformJSDocVariadicType(input *ast.JSDocVariadicType) *ast.Node {
	replacement := tx.Factory().NewArrayTypeNode(tx.Visitor().Visit(input.Type))
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) transformJSDocOptionalType(input *ast.JSDocOptionalType) *ast.Node {
	replacement := tx.Factory().NewUnionTypeNode(tx.Factory().NewNodeList([]*ast.Node{
		tx.Visitor().Visit(input.Type),
		tx.Factory().NewKeywordTypeNode(ast.KindUndefinedKeyword),
	}))
	tx.EmitContext().SetOriginal(replacement, input.AsNode())
	return replacement
}

func (tx *DeclarationTransformer) getNameExpressionPreferringIdentifier(nameExpr *ast.Node) *ast.Node {
	if ast.IsNumericLiteral(nameExpr) {
		// Numeric property names are string properties in JS; convert to string literal
		nameExpr = tx.Factory().NewStringLiteral(nameExpr.Text(), ast.TokenFlagsNone)
	}
	if ast.IsStringLiteralLike(nameExpr) && scanner.IsIdentifierText(nameExpr.Text(), core.LanguageVariantStandard) {
		result := tx.Factory().NewIdentifier(nameExpr.Text()) // prefer non-string literal names where possible
		kwKind := scanner.IdentifierToKeywordKind(result.AsIdentifier())
		// keep keywords as strings, except `default`, which has special reformulations in the transformer
		if kwKind == ast.KindUnknown || kwKind == ast.KindDefaultKeyword {
			// fake this into a parse tree node so the reference resolver resolves the node via `resolveName`
			result.Parent = nameExpr.Parent
			result.Flags &^= ast.NodeFlagsSynthesized
			// intentionally leave Loc unset so the string isn't used as the text source of the identifier
			return result
		}
	}
	return nameExpr
}

func isNotDeclareModifier(mod *ast.Modifier) bool {
	return mod.Kind != ast.KindDeclareKeyword
}

func (tx *DeclarationTransformer) stripDeclareModifiers(node *ast.Node) *ast.Node {
	if node == nil {
		return nil
	}
	mods := node.Modifiers()
	if mods != nil {
		flags := node.ModifierFlags()
		if flags&ast.ModifierFlagsAmbient != 0 {
			filtered := core.Filter(mods.Nodes, isNotDeclareModifier)
			node.AsMutable().SetModifiers(tx.Factory().NewModifierList(filtered))
		}
	}
	return node // no need to recur into children, only strip at top-level
}

func (tx *DeclarationTransformer) visitCJSExportAssignments(expression *ast.Node) *ast.Node {
	if expression != nil {
		_, cleanupDiagnosticContext := tx.setupDiagnosticContext(expression)
		defer cleanupDiagnosticContext()
		switch ast.GetAssignmentDeclarationKind(expression) {
		case ast.JSDeclarationKindModuleExports:
			if tx.state.currentSourceFile.CommonJSModuleIndicator != nil {
				result := tx.transformExportAssignment(expression.Parent, expression, expression.AsBinaryExpression().Right, true /*isExportEquals*/)
				if result != nil {
					tx.cjsExportAssignment = result
					tx.resultHasScopeMarker = true
					tx.resultHasExternalModuleIndicator = true
				}
			}
		}
		return tx.cjsExportAssignmentVisitor.VisitEachChild(expression) // recur through the whole tree, looking for module.exports=
	}
	return nil
}

func (tx *DeclarationTransformer) visitNestedExpression(expression *ast.Node) *ast.Node {
	if expression != nil {
		_, cleanupDiagnosticContext := tx.setupDiagnosticContext(expression)
		defer cleanupDiagnosticContext()
		switch ast.GetAssignmentDeclarationKind(expression) {
		case ast.JSDeclarationKindProperty:
			tx.transformExpandoAssignment(expression.AsBinaryExpression())
		case ast.JSDeclarationKindExportsProperty:
			if tx.state.currentSourceFile.CommonJSModuleIndicator != nil {
				result := tx.transformCommonJSExport(expression, tx.getNameExpressionPreferringIdentifier(ast.GetElementOrPropertyAccessName(expression.AsBinaryExpression().Left)))
				if result != nil {
					tx.cjsExportMembers = append(tx.cjsExportMembers, result)
				}
			}
		case ast.JSDeclarationKindObjectDefinePropertyExports:
			if tx.state.currentSourceFile.CommonJSModuleIndicator != nil {
				result := tx.transformCommonJSExport(expression, tx.getNameExpressionPreferringIdentifier(expression.Arguments()[1]))
				if result != nil {
					tx.cjsExportMembers = append(tx.cjsExportMembers, result)
				}
			}
		}
		return tx.expressionVisitor.VisitEachChild(expression) // recur through the whole tree, looking for special assignments
	}
	return nil
}

func (tx *DeclarationTransformer) transformExpandoAssignment(node *ast.BinaryExpression) {
	left := node.Left

	symbol := node.Symbol
	if symbol == nil || symbol.Flags&ast.SymbolFlagsAssignment == 0 {
		return
	}

	ns := ast.GetLeftmostAccessExpression(left)
	if ns == nil || ns.Kind != ast.KindIdentifier {
		return
	}

	declaration := tx.resolver.GetReferencedValueDeclaration(ns)
	if declaration == nil {
		return
	}

	if tx.shouldStripInternal(declaration) {
		return
	}

	if ast.IsVariableDeclaration(declaration) && declaration.Type() != nil {
		return
	}

	if ast.IsFunctionDeclaration(declaration) && declaration.FunctionLikeData().FullSignature != nil {
		return
	}

	if ast.IsVariableDeclaration(declaration) && !ast.IsFunctionLike(declaration.Initializer()) {
		return // We're going to add a type, no need to dupe members with a namespace
	}

	host := declaration.Symbol()
	if host == nil {
		return
	}

	name := tx.Factory().NewIdentifier(ns.Text())
	property := tx.tryGetPropertyName(left)
	if property == "" || !scanner.IsIdentifierText(property, core.LanguageVariantStandard) {
		return
	}

	if ast.IsDeclaration(declaration) && isDeclarationAndNotVisible(tx.EmitContext(), tx.resolver, declaration) {
		return
	}

	if ast.IsFunctionDeclaration(declaration) && !shouldEmitFunctionProperties(declaration.AsFunctionDeclaration()) {
		return
	}

	tx.transformExpandoHost(name, declaration)

	exportName := tx.Factory().NewIdentifier(property)
	localName := tx.tryGetNameOfAssignedExpression(node.AsNode())
	if localName == nil && !tx.resolver.IsNameResolvable(tx.enclosingDeclaration, property) && !ast.IsNonContextualKeyword(scanner.StringToToken(exportName.Text())) {
		// use exportName as localName if there won't be any conflicts or keyword issues
		localName = exportName
	}
	if localName == nil || ast.IsNonContextualKeyword(scanner.StringToToken(localName.Text())) {
		// fallback to a generated name if the localName doesn't exist or is a keyword
		localName = tx.Factory().NewGeneratedNameForNode(node.AsNode())
	}

	hostId := tx.getExpandoHostId(declaration)
	_, cleanupDiagnosticContext := tx.setupDiagnosticContext(node.AsNode())
	defer cleanupDiagnosticContext()

	if ast.IsIdentifier(node.Right) {
		// alias-like, emit an `export {name}` or `export {name as alias}`
		result := tx.transformBinaryExpressionToExportDeclaration(node.AsNode(), exportName)
		tx.expandoMembers[hostId] = append(tx.expandoMembers[hostId], result)
		return
	}

	preexistingExpandoHasExport := core.Some(tx.expandoMembers[hostId], ast.IsExportDeclaration)
	var varModifiers *ast.ModifierList

	if preexistingExpandoHasExport {
		varModifiers = tx.Factory().NewModifierList(ast.CreateModifiersFromModifierFlags(ast.ModifierFlagsExport, tx.Factory().NewModifier))
	}

	synthesizedNamespace := tx.Factory().NewModuleDeclaration(nil /*modifiers*/, ast.KindNamespaceKeyword, name, tx.Factory().NewModuleBlock(tx.Factory().NewNodeList([]*ast.Node{})))
	synthesizedNamespace.Parent = tx.enclosingDeclaration
	declarationData := synthesizedNamespace.DeclarationData()
	declarationData.Symbol = host
	containerData := synthesizedNamespace.LocalsContainerData()
	containerData.Locals = make(ast.SymbolTable, 0)
	containerData.Locals[localName.Text()] = symbol

	oldEnclosing := tx.enclosingDeclaration
	tx.enclosingDeclaration = synthesizedNamespace
	defer func() {
		tx.enclosingDeclaration = oldEnclosing
	}()

	statements := []*ast.Statement{
		tx.Factory().NewVariableStatement(
			varModifiers,
			tx.Factory().NewVariableDeclarationList(
				tx.Factory().NewNodeList([]*ast.Node{
					tx.Factory().NewVariableDeclaration(localName, nil /*exclamationToken*/, tx.ensureType(node.AsNode(), false), nil /*initializer*/),
				}),
				ast.NodeFlagsNone,
			),
		),
	}

	if localName.Text() != exportName.Text() {
		namedExports := tx.Factory().NewNamedExports(tx.Factory().NewNodeList(
			[]*ast.Node{
				tx.Factory().NewExportSpecifier(false /*isTypeOnly*/, localName, exportName),
			},
		))
		statements = append(statements, tx.Factory().NewExportDeclaration(nil /*modifiers*/, false /*isTypeOnly*/, namedExports, nil /*moduleSpecifier*/, nil /*attributes*/))
	}

	if len(statements) > 1 && !preexistingExpandoHasExport {
		// Add an `export` modifier to all existing expando members so they remain exported after the `export {}` is added
		for _, decl := range tx.expandoMembers[hostId] {
			modifierFlags := ast.ModifierFlagsExport | ast.GetCombinedModifierFlags(decl)
			decl.AsMutable().SetModifiers(tx.Factory().NewModifierList(ast.CreateModifiersFromModifierFlags(modifierFlags, tx.Factory().NewModifier)))
		}
	}
	tx.expandoMembers[hostId] = append(tx.expandoMembers[hostId], statements...)
}

func (tx *DeclarationTransformer) getExpandoHostId(declaration *ast.Declaration) ast.NodeId {
	root := core.IfElse(ast.IsVariableDeclaration(declaration), declaration.Parent.Parent, declaration)
	id := ast.GetNodeId(tx.EmitContext().MostOriginal(root))
	return id
}

func (tx *DeclarationTransformer) transformExpandoHost(name *ast.Node, declaration *ast.Declaration) {
	root := core.IfElse(ast.IsVariableDeclaration(declaration), declaration.Parent.Parent, declaration)
	id := tx.getExpandoHostId(declaration)

	if _, ok := tx.expandoHosts[id]; ok {
		return
	}

	saveNeedsDeclare := tx.needsDeclare
	tx.needsDeclare = true

	modifierFlags := tx.ensureModifierFlags(root)
	defaultExport := modifierFlags&ast.ModifierFlagsExport != 0 && modifierFlags&ast.ModifierFlagsDefault != 0

	tx.needsDeclare = saveNeedsDeclare

	if defaultExport {
		modifierFlags |= ast.ModifierFlagsAmbient
		modifierFlags ^= ast.ModifierFlagsDefault
		modifierFlags ^= ast.ModifierFlagsExport
	}

	_, cleanupDiagnosticContext := tx.setupDiagnosticContext(declaration)
	defer cleanupDiagnosticContext()

	modifiers := tx.Factory().NewModifierList(ast.CreateModifiersFromModifierFlags(modifierFlags, tx.Factory().NewModifier))
	replacement := make([]*ast.Node, 0)

	if ast.IsFunctionDeclaration(declaration) {
		typeParameters, parameters, asteriskToken := extractExpandoHostParams(declaration)
		replacement = append(replacement, tx.Factory().UpdateFunctionDeclaration(declaration.AsFunctionDeclaration(), modifiers, asteriskToken, declaration.Name(), tx.ensureTypeParams(declaration, typeParameters), tx.updateParamList(declaration, parameters), tx.ensureType(declaration, false), nil /*fullSignature*/, nil /*body*/))
	} else if ast.IsVariableDeclaration(declaration) && ast.IsFunctionExpressionOrArrowFunction(declaration.Initializer()) {
		fn := declaration.Initializer()
		typeParameters, parameters, asteriskToken := extractExpandoHostParams(fn)
		replacement = append(replacement, tx.Factory().NewFunctionDeclaration(modifiers, asteriskToken, tx.Factory().NewIdentifier(name.Text()), tx.ensureTypeParams(fn, typeParameters), tx.updateParamList(fn, parameters), tx.ensureType(fn, false), nil /*fullSignature*/, nil /*body*/))
	} else {
		tx.expandoHosts[id] = tx.transformTopLevelDeclaration(declaration)
		return
	}

	tx.state.reportExpandoFunctionErrors(declaration)

	if defaultExport {
		if ast.IsSourceFile(declaration.Parent) {
			tx.resultHasExternalModuleIndicator = true
		}
		tx.resultHasScopeMarker = true
		replacement = append(replacement, tx.Factory().NewExportAssignment(nil /*modifiers*/, false /*isExportEquals*/, nil /*typeNode*/, name))
	}

	// store host result to be added to the output when it's actually visited
	tx.expandoHosts[id] = tx.Factory().NewSyntaxList(replacement)
	if _, ok := tx.lateStatementReplacementMap[id]; ok {
		tx.lateStatementReplacementMap[id] = tx.createFullExpandoBlock(id)
	}
}

func (tx *DeclarationTransformer) createFullExpandoBlock(id ast.NodeId) *ast.Node {
	n := tx.expandoHosts[id]
	if addOns, ok := tx.expandoMembers[id]; ok {
		var modifiers *ast.ModifierList
		var name *ast.Node
		var host []*ast.Node
		if n != nil && n.Kind == ast.KindSyntaxList {
			// find the first named syntax list element and use its' name & modifiers
			for c := range n.AsSyntaxList().IterChildren() {
				if c.Name() != nil {
					name = c.Name().Clone(tx.Factory())
					if c.Modifiers() != nil {
						modifiers = c.Modifiers().Clone(tx.Factory().AsNodeFactory())
					}
					break
				}
			}
			host = n.AsSyntaxList().Children
		} else if n != nil {
			name = n.Name().Clone(tx.Factory())
			if n.Modifiers() != nil {
				modifiers = n.Modifiers().Clone(tx.Factory().AsNodeFactory())
			}
			host = []*ast.Node{n}
		}
		if name != nil {
			moduleDecl := tx.Factory().NewModuleDeclaration(
				modifiers,
				ast.KindNamespaceKeyword,
				name,
				tx.Factory().NewModuleBlock(tx.Factory().NewNodeList(addOns)),
			)
			members := append(host, moduleDecl)
			return tx.Factory().NewSyntaxList(members)
		}
	}
	return n
}

func extractExpandoHostParams(node *ast.Node) (typeParameters *ast.TypeParameterList, parameters *ast.ParameterList, asteriskToken *ast.TokenNode) {
	switch node.Kind {
	case ast.KindFunctionExpression:
		fn := node.AsFunctionExpression()
		return fn.TypeParameters, fn.Parameters, fn.AsteriskToken
	case ast.KindArrowFunction:
		fn := node.AsArrowFunction()
		return fn.TypeParameters, fn.Parameters, fn.AsteriskToken
	default:
		fn := node.AsFunctionDeclaration()
		return fn.TypeParameters, fn.Parameters, fn.AsteriskToken
	}
}

func (tx *DeclarationTransformer) tryGetPropertyName(node *ast.Node) string {
	if ast.IsElementAccessExpression(node) {
		return tx.resolver.GetElementAccessExpressionName(node.AsElementAccessExpression())
	}
	if ast.IsPropertyAccessExpression(node) {
		return node.Name().Text()
	}
	return ""
}
