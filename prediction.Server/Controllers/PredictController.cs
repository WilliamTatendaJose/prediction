using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using System.Text;
using System.Text.Json;
using Microsoft.AspNetCore.SignalR;
using Microsoft.Extensions.ML;
using prediction.Server.Hubs;
using Prediction_Server;

namespace prediction.Server.Controllers
{
    [Route("api/[controller]")]
    [ApiController]
    public class PredictController : ControllerBase
    {
        private readonly PredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput> _predictionEnginePool;
        private readonly IHubContext<AccessControlHub> _hubContext;
        private static PredictionModel.ModelOutput? _latestPrediction;
         public PredictController(PredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput> predictionEnginePool, IHubContext<AccessControlHub>hubContext)
        {
            _predictionEnginePool = predictionEnginePool;
            _hubContext = hubContext;
        }

        [HttpGet(Name ="GetPrediction")]
        public IActionResult GetPrediction()
        {
            if (_latestPrediction == null)
            {
                return NotFound("Not Prediction Yet");
            }

            var data = JsonSerializer.Serialize<PredictionModel.ModelOutput>(_latestPrediction);

            return Ok(data);
        }

        [HttpPost(Name = "Prediction")]
        public async Task<IActionResult> Prediction([FromBody] PredictionModel.ModelInput input)

        {

            if (input == null)
            {
                return BadRequest("Input data is required.");
            }

            var prediction = await Task.FromResult(_predictionEnginePool.Predict(input));
            _latestPrediction = prediction;

            var data = JsonSerializer.Serialize<PredictionModel.ModelOutput>(prediction);
            await _hubContext.Clients.All.SendAsync("Prediction", data);

            Console.WriteLine(data);

            return Ok(prediction);
        }



    }
}
