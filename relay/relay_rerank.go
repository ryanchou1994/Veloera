// Copyright (c) 2025 Tethys Plex
//
// This file is part of Veloera.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"veloera/common"
	"veloera/dto"
	relaycommon "veloera/relay/common"
	"veloera/relay/helper"
	"veloera/service"
)

func getRerankPromptToken(rerankRequest dto.RerankRequest) int {
	token, _ := service.CountTokenInput(rerankRequest.Query, rerankRequest.Model)
	for _, document := range rerankRequest.Documents {
		tkm, err := service.CountTokenInput(document, rerankRequest.Model)
		if err == nil {
			token += tkm
		}
	}
	return token
}

func RerankHelper(c *gin.Context, relayMode int) (openaiErr *dto.OpenAIErrorWithStatusCode) {

	var rerankRequest *dto.RerankRequest
	err := common.UnmarshalBodyReusable(c, &rerankRequest)
	if err != nil {
		common.LogError(c, fmt.Sprintf("getAndValidateTextRequest failed: %s", err.Error()))
		return service.OpenAIErrorWrapperLocal(err, "invalid_text_request", http.StatusBadRequest)
	}

	relayInfo := relaycommon.GenRelayInfoRerank(c, rerankRequest)

	if rerankRequest.Query == "" {
		return service.OpenAIErrorWrapperLocal(fmt.Errorf("query is empty"), "invalid_query", http.StatusBadRequest)
	}
	if len(rerankRequest.Documents) == 0 {
		return service.OpenAIErrorWrapperLocal(fmt.Errorf("documents is empty"), "invalid_documents", http.StatusBadRequest)
	}

	err = helper.ModelMappedHelper(c, relayInfo)
	if err != nil {
		return service.OpenAIErrorWrapperLocal(err, "model_mapped_error", http.StatusInternalServerError)
	}

	rerankRequest.Model = relayInfo.UpstreamModelName

	promptToken := getRerankPromptToken(*rerankRequest)
	relayInfo.PromptTokens = promptToken

	priceData, err := helper.ModelPriceHelper(c, relayInfo, promptToken, 0)
	if err != nil {
		return service.OpenAIErrorWrapperLocal(err, "model_price_error", http.StatusInternalServerError)
	}
	// pre-consume quota 预消耗配额
	preConsumedQuota, userQuota, openaiErr := preConsumeQuota(c, priceData.ShouldPreConsumedQuota, relayInfo)
	if openaiErr != nil {
		return openaiErr
	}
	defer func() {
		if openaiErr != nil {
			returnPreConsumedQuota(c, relayInfo, userQuota, preConsumedQuota)
		}
	}()

	adaptor := GetAdaptor(relayInfo.ApiType)
	if adaptor == nil {
		return service.OpenAIErrorWrapperLocal(fmt.Errorf("invalid api type: %d", relayInfo.ApiType), "invalid_api_type", http.StatusBadRequest)
	}
	adaptor.Init(relayInfo)

	convertedRequest, err := adaptor.ConvertRerankRequest(c, relayInfo.RelayMode, *rerankRequest)
	if err != nil {
		return service.OpenAIErrorWrapperLocal(err, "convert_request_failed", http.StatusInternalServerError)
	}
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return service.OpenAIErrorWrapperLocal(err, "json_marshal_failed", http.StatusInternalServerError)
	}
	requestBody := bytes.NewBuffer(jsonData)
	statusCodeMappingStr := c.GetString("status_code_mapping")
	resp, err := adaptor.DoRequest(c, relayInfo, requestBody)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}

	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if httpResp.StatusCode != http.StatusOK {
			openaiErr = service.RelayErrorHandler(httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(openaiErr, statusCodeMappingStr)
			return openaiErr
		}
	}

	usage, openaiErr := adaptor.DoResponse(c, httpResp, relayInfo)
	if openaiErr != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(openaiErr, statusCodeMappingStr)
		return openaiErr
	}
	postConsumeQuota(c, relayInfo, usage.(*dto.Usage), preConsumedQuota, userQuota, priceData, "")
	return nil
}
