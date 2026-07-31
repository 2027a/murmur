package textpipe

type Intent string

const (
	OpenQuote      Intent = "open_quote"
	CloseQuote     Intent = "close_quote"
	LineBreak      Intent = "line_break"
	ParagraphBreak Intent = "paragraph_break"
)

var Intents = []Intent{OpenQuote, CloseQuote, LineBreak, ParagraphBreak}
